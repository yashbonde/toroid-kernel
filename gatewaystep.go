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
// Client (LiteLLM gateway, OpenAI direct). No third-party model SDK. The
// kernel keeps owning tools, turns, and compaction; GatewayStep performs the
// one call and prices it from the model's catalog rates.
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

// usageFrom copies the wire token counts and prices the step.
//
// The gateway's own cost header wins when present (X-LLM-Gateway-Cost in
// micro-USD, or the LiteLLM x-litellm-response-cost in USD): it is the
// biller's authoritative number, already reflecting per-deployment rates,
// discounts, and margin, none of which a client-side catalog can know. When
// the gateway reports nothing, cost stays honestly unknown (PricingOK false)
// rather than silently zero. There is no client-side rate table any more; the
// gateway — or a direct provider — is the only source of cost.
func usageFrom(_ Model, wire llm.Usage, gw llm.GatewayInfo) Usage {
	u := Usage{
		Input:      wire.Input,
		Output:     wire.Output,
		Reasoning:  wire.Reasoning,
		CacheRead:  wire.CacheRead,
		CacheWrite: wire.CacheWrite,
	}
	if gw.CostUSD != nil {
		u.Cost = *gw.CostUSD
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
		Usage:      usageFrom(model, resp.Usage, resp.Gateway),
		StopReason: stopReasonFromFinish(resp.FinishReason),
	}, nil
}

// CompleteObject runs one structured-output llm-step as a forced tool call:
// the schema is sent as the request's single tool and tool_choice forces the
// model to call it, so the object arrives as that call's arguments.
func (s *GatewayStep) CompleteObject(ctx context.Context, model Model, c Context, sch Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error) {
	if schemaDescription == "" {
		schemaDescription = "Record your response using this structured schema."
	}
	req := buildRequest(model, c, opts)
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
		Usage:      usageFrom(model, resp.Usage, resp.Gateway),
		StopReason: stopReasonFromFinish(resp.FinishReason),
	}, nil
}

// Stream runs one streaming llm-step. It forwards text/reasoning deltas on the
// returned channel and assembles the final assistant message. If ctx is
// cancelled mid-step the partial content is preserved and StopReason is
// "aborted" so the caller can keep or continue it.
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
			u = usageFrom(model, resp.Usage, resp.Gateway)
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
