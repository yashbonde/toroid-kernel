package toroid

import (
	"context"
	"errors"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"
)

// FantasyStep is the first Step backend: it maps one product llm-step onto a
// single Fantasy LanguageModel call (Generate / Stream / GenerateObject), never
// the multi-step Agent loop. The kernel keeps owning tools, turns, and
// compaction; FantasyStep just performs the one call and prices its usage from
// the catalog (M2 honesty preserved). A future openai-completions client
// (Phase C) can implement Step and drop in here unchanged.
type FantasyStep struct {
	LM fantasy.LanguageModel
}

// compile-time assertion that FantasyStep satisfies Step.
var _ Step = (*FantasyStep)(nil)

// NewFantasyStep wraps a Fantasy language model as a Step.
func NewFantasyStep(lm fantasy.LanguageModel) *FantasyStep { return &FantasyStep{LM: lm} }

// buildPrompt turns a Context into a Fantasy prompt: a single leading system
// message (M3 — never duplicated in the message list) followed by the messages.
func buildPrompt(c Context) fantasy.Prompt {
	msgs := make([]fantasy.Message, 0, len(c.Messages)+1)
	if c.System != "" {
		msgs = append(msgs, fantasy.NewSystemMessage(c.System))
	}
	msgs = append(msgs, c.Messages...)
	return msgs
}

// buildTools converts kernel AgentTools into the low-level FunctionTools a raw
// Call carries. Mirrors Fantasy's own agent.prepareTools so the wire shape is
// identical whether the kernel loops via Agent or via Step.
func buildTools(tools []fantasy.AgentTool) []fantasy.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]fantasy.Tool, 0, len(tools))
	for _, t := range tools {
		info := t.Info()
		inputSchema := map[string]any{
			"type":       "object",
			"properties": info.Parameters,
			"required":   info.Required,
		}
		schema.Normalize(inputSchema)
		out = append(out, fantasy.FunctionTool{
			Name:            info.Name,
			Description:     info.Description,
			InputSchema:     inputSchema,
			ProviderOptions: t.ProviderOptions(),
		})
	}
	return out
}

// firePayload invokes the debug hook (M10) with a redaction-friendly view.
func firePayload(model Model, c Context, opts StepOptions, schemaName string, stream bool) {
	if opts.OnPayload == nil {
		return
	}
	opts.OnPayload(PayloadDebug{
		Model:    model.ID,
		Provider: model.Provider,
		API:      model.API,
		System:   c.System,
		Messages: c.Messages,
		Tools:    toolNames(c.Tools),
		Schema:   schemaName,
		Stream:   stream,
	})
}

// Complete runs one non-streaming llm-step.
func (s *FantasyStep) Complete(ctx context.Context, model Model, c Context, opts StepOptions) (*AssistantMessage, error) {
	firePayload(model, c, opts, "", false)
	// Non-streaming: attach a sink so the gateway's authoritative cost header
	// (when present) is preferred over the local estimate (Phase C).
	ctx, _ = withCostSink(ctx)
	resp, err := s.LM.Generate(ctx, fantasy.Call{
		Prompt:          buildPrompt(c),
		Tools:           buildTools(c.Tools),
		MaxOutputTokens: opts.MaxOutputTokens,
	})
	if err != nil {
		return nil, err
	}
	var u Usage
	u.FromFantasyUsage(resp.Usage, model.ID)
	applyGatewayCost(ctx, &u)
	return &AssistantMessage{
		Content:    resp.Content,
		Usage:      u,
		StopReason: stopReasonFromFinish(resp.FinishReason),
	}, nil
}

// CompleteObject runs one structured-output llm-step (M7). Its usage is priced
// so the schema pass is a first-class, costed llm-step — the caller must record
// the returned Usage so the object call shows up in transcript totals.
func (s *FantasyStep) CompleteObject(ctx context.Context, model Model, c Context, sch fantasy.Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error) {
	firePayload(model, c, opts, schemaName, false)
	ctx, _ = withCostSink(ctx) // non-streaming: prefer gateway cost when reported
	resp, err := s.LM.GenerateObject(ctx, fantasy.ObjectCall{
		Prompt:            buildPrompt(c),
		Schema:            sch,
		SchemaName:        schemaName,
		SchemaDescription: schemaDescription,
		MaxOutputTokens:   opts.MaxOutputTokens,
	})
	if err != nil {
		return nil, err
	}
	var u Usage
	u.FromFantasyUsage(resp.Usage, model.ID)
	applyGatewayCost(ctx, &u)
	return &ObjectResult{
		Object:     resp.Object,
		RawText:    resp.RawText,
		Usage:      u,
		StopReason: stopReasonFromFinish(resp.FinishReason),
	}, nil
}

// Stream runs one streaming llm-step. It forwards text/reasoning deltas on the
// returned channel and assembles the final assistant message. If ctx is
// cancelled mid-step the partial content is preserved and StopReason is
// "aborted" (M11) so the caller can keep or continue it.
func (s *FantasyStep) Stream(ctx context.Context, model Model, c Context, opts StepOptions) (*StreamResult, error) {
	firePayload(model, c, opts, "", true)
	stream, err := s.LM.Stream(ctx, fantasy.Call{
		Prompt:          buildPrompt(c),
		Tools:           buildTools(c.Tools),
		MaxOutputTokens: opts.MaxOutputTokens,
	})
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
		var text string
		var usage fantasy.Usage
		finish := fantasy.FinishReasonUnknown
		aborted := false

		for part := range stream {
			// Honour cancellation between parts: stop consuming, keep partial text.
			select {
			case <-ctx.Done():
				aborted = true
			default:
			}
			if aborted {
				break
			}

			switch part.Type {
			case fantasy.StreamPartTypeTextDelta:
				text += part.Delta
				select {
				case deltas <- StreamDelta{Text: part.Delta}:
				case <-ctx.Done():
					aborted = true
				}
			case fantasy.StreamPartTypeReasoningDelta:
				select {
				case deltas <- StreamDelta{Reasoning: part.Delta}:
				case <-ctx.Done():
					aborted = true
				}
			case fantasy.StreamPartTypeFinish:
				usage = part.Usage
				finish = part.FinishReason
			case fantasy.StreamPartTypeError:
				if part.Error != nil {
					finalErr = part.Error
				}
			}
			if aborted {
				break
			}
		}

		var u Usage
		u.FromFantasyUsage(usage, model.ID)
		reason := stopReasonFromFinish(finish)
		if aborted || (finalErr == nil && ctx.Err() != nil) {
			reason = StopReasonAborted
			if finalErr == nil {
				finalErr = ctx.Err()
			}
		}
		if finalErr != nil && !errors.Is(finalErr, context.Canceled) && reason != StopReasonAborted {
			reason = StopReasonError
		}
		final = AssistantMessage{
			Content:    fantasy.ResponseContent{fantasy.TextContent{Text: text}},
			Usage:      u,
			StopReason: reason,
		}
	}()

	return &StreamResult{
		Deltas: deltas,
		Final: func() (*AssistantMessage, error) {
			<-done
			return &final, finalErr
		},
	}, nil
}
