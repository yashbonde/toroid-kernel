package toroid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

// GatewayStep is the production Step backend: it maps one product llm-step
// onto a single wire call via an in-repo llm client — the OpenAI-compatible
// Client (LiteLLM gateway, OpenAI direct) or the native AnthropicClient. No
// third-party model SDK. The kernel keeps owning tools, turns, and compaction;
// GatewayStep performs the one call and prices it (gateway cost header when
// present, catalog rates otherwise).
type GatewayStep struct {
	Client llm.Chat
}

var _ Step = (*GatewayStep)(nil)

// NewGatewayStep wraps a model wire as a Step.
func NewGatewayStep(client llm.Chat) *GatewayStep { return &GatewayStep{Client: client} }

// buildRequest maps a Step call onto one llm.Request.
func buildRequest(model Model, c Context, opts StepOptions) llm.Request {
	req := llm.Request{
		Model:        model.ID,
		SystemPrefix: c.SystemPrefix,
		System:       c.System,
		Messages:     c.Messages,
		Tools:        c.Tools,
		Metadata:     c.Metadata,
		CachePrompt:  model.PromptCache && !opts.DisablePromptCache,
	}
	if opts.MaxOutputTokens != nil {
		mt := int(*opts.MaxOutputTokens)
		req.MaxTokens = &mt
	}
	switch opts.Thinking {
	case ThinkingLow:
		req.ReasoningEffort = "low"
	case ThinkingHigh:
		req.ReasoningEffort = "high"
	}
	return req
}

// usageFrom copies the wire token counts and prices the step: the gateway's
// reported cost wins when present (non-stream via LiteLLM); otherwise the
// catalog's cached per-token rates price it (direct provider routes,
// streaming). With neither, cost stays honestly unknown.
func usageFrom(model Model, wire llm.Usage, gatewayCost *float64) Usage {
	u := Usage{
		Input:      wire.Input,
		Output:     wire.Output,
		Reasoning:  wire.Reasoning,
		CacheRead:  wire.CacheRead,
		CacheWrite: wire.CacheWrite,
	}
	switch {
	case gatewayCost != nil:
		u.Cost = *gatewayCost
		u.PricingOK = true
	case model.Price != nil:
		p := model.Price
		u.Cost = float64(u.Input)*p.In + float64(u.Output)*p.Out +
			float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite
		u.PricingOK = true
	}
	return u
}

// Complete runs one non-streaming llm-step.
func (s *GatewayStep) Complete(ctx context.Context, model Model, c Context, opts StepOptions) (*AssistantMessage, error) {
	resp, err := s.Client.Complete(ctx, buildRequest(model, c, opts))
	if err != nil {
		return nil, err
	}
	return &AssistantMessage{
		Content:    resp.Content,
		Usage:      usageFrom(model, resp.Usage, resp.GatewayCostUSD),
		StopReason: stopReasonFromFinish(resp.FinishReason),
	}, nil
}

// CompleteObject runs one structured-output llm-step as a forced tool call:
// the schema is sent as the request's single tool and tool_choice forces the
// model to call it, so the object arrives as that call's arguments. This works
// uniformly across LiteLLM upstreams (including Bedrock-backed Anthropic,
// which rejects tool-bearing history without a tools= param).
func (s *GatewayStep) CompleteObject(ctx context.Context, model Model, c Context, sch Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error) {
	if schemaDescription == "" {
		schemaDescription = "Record your response using this structured schema."
	}
	req := buildRequest(model, c, opts)
	req.CachePrompt = false // one-shot: different tool set, prefix never re-read
	req.Tools = []llm.Tool{{Name: schemaName, Description: schemaDescription, Parameters: sch}}
	req.ToolChoice = schemaName
	resp, err := s.Client.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	// Prefer the forced tool call's arguments; fall back to text for upstreams
	// that answer with a plain JSON body despite the forced choice (stripping a
	// markdown fence if one wraps it).
	raw := ""
	if calls := resp.ToolCalls(); len(calls) > 0 {
		raw = strings.TrimSpace(calls[0].Arguments)
	} else {
		raw = strings.TrimSpace(resp.Text())
		if strings.HasPrefix(raw, "```") {
			if i := strings.Index(raw, "\n"); i >= 0 {
				raw = raw[i+1:]
			}
			raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "```"))
		}
	}
	var obj any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		if len(raw) > 200 {
			raw = raw[:200] + "…"
		}
		return nil, fmt.Errorf("structured output is not valid JSON: %w (%s)", err, raw)
	}
	return &ObjectResult{
		Object:     obj,
		RawText:    raw,
		Usage:      usageFrom(model, resp.Usage, resp.GatewayCostUSD),
		StopReason: stopReasonFromFinish(resp.FinishReason),
	}, nil
}

// Stream runs one streaming llm-step. It forwards text/reasoning deltas on the
// returned channel and assembles the final assistant message. If ctx is
// cancelled mid-step the partial content is preserved and StopReason is
// "aborted" so the caller can keep or continue it. Streaming responses carry
// no gateway cost header, so Usage has tokens only.
func (s *GatewayStep) Stream(ctx context.Context, model Model, c Context, opts StepOptions) (*StreamResult, error) {
	stream, err := s.Client.StreamComplete(ctx, buildRequest(model, c, opts))
	if err != nil {
		return nil, err
	}

	deltas := make(chan StreamDelta)
	done := make(chan struct{})
	var final AssistantMessage
	var finalErr error

	go func() {
		defer close(done)
		defer close(deltas)
		for d := range stream.Deltas {
			select {
			case deltas <- StreamDelta{Text: d.Text, Reasoning: d.Reasoning}:
			case <-ctx.Done():
				// Keep draining the client stream (it honours ctx itself) but stop
				// forwarding; partial content is preserved in the final response.
			}
		}
		resp, err := stream.Final()
		var u Usage
		reason := StopReasonOther
		if resp != nil {
			u = usageFrom(model, resp.Usage, nil)
			reason = stopReasonFromFinish(resp.FinishReason)
		}
		if errors.Is(err, context.Canceled) || (err == nil && ctx.Err() != nil) {
			reason = StopReasonAborted
			if err == nil {
				err = ctx.Err()
			}
		} else if err != nil {
			reason = StopReasonError
		}
		var content []llm.Part
		if resp != nil {
			content = resp.Content
		}
		final = AssistantMessage{Content: content, Usage: u, StopReason: reason}
		finalErr = err
	}()

	return &StreamResult{
		Deltas: deltas,
		Final: func() (*AssistantMessage, error) {
			<-done
			return &final, finalErr
		},
	}, nil
}
