package toroid

import (
	"context"

	"charm.land/fantasy"
)

// Step layer (Phase B). A Step performs exactly one LLM call — one product
// "llm-step" (see assets/terminology.md). It never runs the tool loop: the
// kernel owns turns, tools, compaction, and subagents and drives a Step once per
// turn. This is the architectural split the port targets — a thin step API under
// the existing chat loop — while Fantasy remains the first backend (FantasyStep).
//
// Only the OpenAI-compatible LiteLLM gateway is in scope; a Step implementation
// is deliberately protocol-agnostic so a future openai-completions client
// (Phase C) can replace FantasyStep without touching the kernel.

// StopReason is the product-facing reason an llm-step ended. It normalizes the
// backend's finish reason to a stable, small vocabulary.
const (
	StopReasonStop    = "stop"    // model produced a final answer
	StopReasonToolUse = "toolUse" // model wants tools run before continuing
	StopReasonLength  = "length"  // hit the max output token budget
	StopReasonError   = "error"   // provider/gateway error ended the step
	StopReasonAborted = "aborted" // caller cancelled mid-step (ctx cancel / ESC)
	StopReasonOther   = "other"   // anything else (content filter, unknown)
)

// stopReasonFromFinish maps a Fantasy finish reason to a product StopReason.
func stopReasonFromFinish(fr fantasy.FinishReason) string {
	switch fr {
	case fantasy.FinishReasonStop:
		return StopReasonStop
	case fantasy.FinishReasonToolCalls:
		return StopReasonToolUse
	case fantasy.FinishReasonLength:
		return StopReasonLength
	case fantasy.FinishReasonError:
		return StopReasonError
	default:
		// content-filter, other, unknown, and the empty reason all collapse here.
		return StopReasonOther
	}
}

// Context is the caller-owned input to one llm-step (M3): a single system blob,
// the message list (which must NOT duplicate the system prompt), and the tools
// available for this call. Keeping system out of Messages preserves a stable
// cache prefix and avoids double-billing the system text.
type Context struct {
	System   string
	Messages []fantasy.Message
	Tools    []fantasy.AgentTool
}

// AssistantMessage is the result of one non-streaming llm-step: the assistant
// content, its billed Usage (tokens + Cost + PricingOK, M2), and why it stopped.
type AssistantMessage struct {
	Content    fantasy.ResponseContent
	Usage      Usage
	StopReason string
}

// Text returns the assistant text of the message.
func (m *AssistantMessage) Text() string { return m.Content.Text() }

// ObjectResult is the result of one structured-output llm-step (M7): the parsed
// object, its raw JSON text, and billed Usage — so a schema pass is a first-class,
// costed llm-step rather than an invisible extra call.
type ObjectResult struct {
	Object     any
	RawText    string
	Usage      Usage
	StopReason string
}

// PayloadDebug is a redaction-friendly view of an outbound llm-step request,
// handed to StepOptions.OnPayload for inspection/logging (M10). It intentionally
// omits raw provider bytes; it is enough to verify tools, schema, and the
// single-system-prompt invariant on the wire.
type PayloadDebug struct {
	Model    string
	Provider string
	API      string
	System   string
	Messages []fantasy.Message
	Tools    []string // tool names attached to the call
	Schema   string   // schema name for object calls; "" otherwise
	Stream   bool
}

// StepOptions carry per-call knobs shared by every Step method.
type StepOptions struct {
	MaxOutputTokens *int64
	Thinking        Thinking
	// OnPayload, when set, is invoked with a debug view of the request just
	// before it is sent (M10). Best-effort and synchronous; keep it cheap.
	OnPayload func(PayloadDebug)
}

// StreamDelta is one incremental chunk of a streaming llm-step.
type StreamDelta struct {
	Text      string // assistant text delta
	Reasoning string // reasoning/thinking delta
}

// StreamResult is a live streaming llm-step. Callers range over Deltas until it
// closes, then call Final for the assembled AssistantMessage (partial content is
// preserved when the context was cancelled mid-step, M11).
type StreamResult struct {
	Deltas <-chan StreamDelta
	// Final blocks until the stream is fully drained and returns the assembled
	// assistant message plus any terminal error. Safe to call after Deltas closes.
	Final func() (*AssistantMessage, error)
}

// Step performs exactly one LLM call. Implementations must never run a tool loop.
type Step interface {
	// Complete runs one non-streaming llm-step and returns the assistant message.
	Complete(ctx context.Context, model Model, c Context, opts StepOptions) (*AssistantMessage, error)
	// Stream runs one streaming llm-step, delivering deltas as they arrive.
	Stream(ctx context.Context, model Model, c Context, opts StepOptions) (*StreamResult, error)
	// CompleteObject runs one structured-output llm-step against a schema.
	CompleteObject(ctx context.Context, model Model, c Context, schema fantasy.Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error)
}

// toolNames extracts the display names of a call's tools for PayloadDebug.
func toolNames(tools []fantasy.AgentTool) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Info().Name)
	}
	return names
}
