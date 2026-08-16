package toroid

import (
	"context"
	"reflect"

	"github.com/yashbonde/toroid-kernel/llm"
)

// Step layer. A Step performs exactly one LLM call — one product "llm-step"
// (see assets/terminology.md). It never runs the tool loop: the kernel owns
// turns, tools, compaction, and subagents and drives a Step once per turn.
// The production implementation is GatewayStep over the in-repo llm client;
// FauxStep is the scripted in-memory backend for tests.

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

// stopReasonFromFinish maps an llm finish reason to a product StopReason.
func stopReasonFromFinish(fr llm.FinishReason) string {
	switch fr {
	case llm.FinishStop:
		return StopReasonStop
	case llm.FinishToolCalls:
		return StopReasonToolUse
	case llm.FinishLength:
		return StopReasonLength
	case llm.FinishError:
		return StopReasonError
	default:
		// content-filter, other, unknown, and the empty reason all collapse here.
		return StopReasonOther
	}
}

// Schema is a JSON-Schema object (as a map) describing a structured-output
// shape. Build one by hand or from a Go type via GenerateSchema.
type Schema = map[string]any

// GenerateSchema builds a Schema from a Go type's reflection info (json tags,
// description tags). Convenience re-export of llm.GenerateSchema for hosts.
func GenerateSchema(t reflect.Type) Schema { return llm.GenerateSchema(t) }

// Context is the caller-owned input to one llm-step: a single system blob, the
// message list (which must NOT duplicate the system prompt), and the tools
// available for this call. Keeping system out of Messages preserves a stable
// cache prefix and avoids double-billing the system text.
type Context struct {
	SystemPrefix string // invariant, independently cacheable instructions
	System       string // run-specific system context
	Messages     []llm.Message
	Tools        []llm.Tool
	Metadata     llm.RequestMetadata
}

// AssistantMessage is the result of one non-streaming llm-step: the assistant
// content, its Usage (tokens + gateway-reported cost), and why it stopped.
type AssistantMessage struct {
	Content    []llm.Part
	Usage      Usage
	StopReason string
}

// Text returns the assistant text of the message.
func (m *AssistantMessage) Text() string { return llm.TextOf(m.Content) }

// ToolCalls returns the tool calls in the message.
func (m *AssistantMessage) ToolCalls() []llm.ToolCallPart { return llm.ToolCallsOf(m.Content) }

// ObjectResult is the result of one structured-output llm-step: the parsed
// object, its raw JSON text, and Usage — a schema pass is a first-class,
// costed llm-step rather than an invisible extra call.
type ObjectResult struct {
	Object     any
	RawText    string
	Usage      Usage
	StopReason string
}

// StepOptions carry per-call knobs shared by every Step method.
type StepOptions struct {
	MaxOutputTokens *int64
	Thinking        Thinking
	// DisablePromptCache skips cache_control breakpoints for this call. Set on
	// one-shot llm-steps (compaction, the schema pass): their tool set differs
	// from the loop's, so the prefix cannot hit the loop's cache, and stamping
	// breakpoints would pay the cache-write premium for a prefix never re-read.
	DisablePromptCache bool
}

// StreamDelta is one incremental chunk of a streaming llm-step.
type StreamDelta struct {
	Text      string // assistant text delta
	Reasoning string // reasoning/thinking delta
}

// StreamResult is a live streaming llm-step. Callers range over Deltas until it
// closes, then call Final for the assembled AssistantMessage (partial content is
// preserved when the context was cancelled mid-step).
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
	CompleteObject(ctx context.Context, model Model, c Context, schema Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error)
}
