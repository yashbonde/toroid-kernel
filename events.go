package toroid

import "encoding/json"

type EventKind string

const (
	EventSessionStart       EventKind = "SessionStart"
	EventUserPromptSubmit   EventKind = "UserPromptSubmit"
	EventPreToolUse         EventKind = "PreToolUse"         // before a tool is called
	EventPostToolUse        EventKind = "PostToolUse"        // after a tool call is completed
	EventPostToolUseFailure EventKind = "PostToolUseFailure" // after a tool call fails
	EventSubagentStart      EventKind = "SubagentStart"      // before the subagent is started
	EventSubagentStop       EventKind = "SubagentStop"       // after the subagent finishes, sync or async (Payload.Async)
	EventMasterIdle         EventKind = "MasterIdle"         // after the main agent is idle
	EventReasoning          EventKind = "Reasoning"          // streamed reasoning/thinking tokens (display only, not stored)
	EventTurnStarted        EventKind = "TurnStarted"        // before one agent loop turn begins; carries the outbound llm-step shape
	EventTurnCompleted      EventKind = "TurnCompleted"      // after the LLM response and any requested tools finish; carries content and cost
	EventTurnFailed         EventKind = "TurnFailed"         // when a turn cannot reach its normal boundary
	EventGuardTriggered     EventKind = "GuardTriggered"     // a loop guard acted: inspection/validation/completion nudge, repeat-call guard, or MaxIter
	EventStop               EventKind = "Stop"               // when the agent is stopped
	EventPreCompact         EventKind = "PreCompact"         // before compacting the memory
	EventPostCompact        EventKind = "PostCompact"        // after compaction; payload contains the LLM-generated summary
	EventSessionEnd         EventKind = "SessionEnd"         // after the session ends
	EventQueueInterrupt     EventKind = "QueueInterrupt"     // fired when queued messages interrupt the stream at a step boundary
)

// Event is the envelope fired for every kernel occurrence. TranscriptID,
// ChatID, TurnID, and LLMStepID identify where in the transcript -> chat ->
// turn -> llm-step scope hierarchy this event sits; they are set whenever the
// scope is known and left empty otherwise (e.g. SessionStart/SessionEnd have
// no chat yet). Every event kind carries them uniformly, so a consumer can
// fold the flat stream back into the scope tree without special-casing which
// kinds happen to carry ids in their payload.
type Event struct {
	Kind         EventKind `json:"kind"`
	SessionID    string    `json:"session_id"`
	TraceID      string    `json:"trace_id"`
	SpanID       string    `json:"span_id"`
	TranscriptID string    `json:"transcript_id,omitempty"`
	ChatID       string    `json:"chat_id,omitempty"`
	TurnID       string    `json:"turn_id,omitempty"`
	LLMStepID    string    `json:"llm_step_id,omitempty"`
	EmitTS       int64     `json:"emit_ts"` // UnixNano wall clock
	Seq          uint64    `json:"seq"`     // monotonic counter within a span
	Payload      any       `json:"payload,omitempty"`
}

// nonObservableKinds are display-only and control-plane signals that are not part
// of the observability trace: reasoning deltas and idle/queue bookkeeping. They
// are excluded from OTEL span events so the exported trace carries only
// meaningful work, not UI chatter.
var nonObservableKinds = map[EventKind]bool{
	EventReasoning:      true, // streamed thinking deltas (also not persisted)
	EventMasterIdle:     true,
	EventQueueInterrupt: true,
}

// Observable reports whether an event of this kind belongs in the observability
// trace (and therefore in OTEL span events). Display-only and control-plane kinds
// return false.
func (k EventKind) Observable() bool { return !nonObservableKinds[k] }

// OTEL renders the event as an OpenTelemetry span event: a point-in-time
// annotation carrying the event name, wall-clock timestamp, and the full payload
// serialized as a JSON attribute. This is the single canonical mapping used both
// when persisting (Save:true) and when exporting via OTELSpans, so the stored and
// exported shapes can never drift.
func (e Event) OTEL() OTELEvent {
	oe := OTELEvent{Name: string(e.Kind), TimeUnix: e.EmitTS}
	if e.Payload != nil {
		if b, err := json.Marshal(e.Payload); err == nil && len(b) > 0 && string(b) != "null" {
			oe.Attribute = string(b)
		}
	}
	return oe
}

// Payload types

type UserPromptPayload struct {
	Prompt string `json:"prompt"`
}

type ReasoningPayload struct {
	Text string `json:"text"`
}

// TurnPayload is attached to EventTurnStarted, EventTurnCompleted, and
// EventTurnFailed — the one event describing everything about a turn.
//
// On EventTurnStarted: Model, Messages, Tools, Schema describe the outbound
// llm-step about to be sent (what EventLLMStep used to carry separately).
//
// On EventTurnCompleted: StopReason, ToolCalls, Messages (the turn's full
// structured content, JSON-serialized []llm.Message — what EventAssistantTurn
// used to carry) and TurnUsage/TurnCostUSD/TotalCostUSD (what EventTurnCost
// used to carry) are populated.
//
// On EventTurnFailed: StopReason and Error are populated.
type TurnPayload struct {
	StopReason string `json:"stop_reason,omitempty"`
	ToolCalls  int    `json:"tool_calls,omitempty"`
	Error      string `json:"error,omitempty"`

	// TurnStarted fields
	Model    string   `json:"model,omitempty"`
	Messages int      `json:"messages,omitempty"` // outbound message count (system excluded)
	Tools    []string `json:"tools,omitempty"`
	Schema   string   `json:"schema,omitempty"`

	// TurnCompleted fields
	Content      json.RawMessage `json:"content,omitempty"` // []llm.Message, full structured content for the turn
	TurnUsage    Usage           `json:"turn_usage,omitempty"`
	TurnCostUSD  float64         `json:"turn_cost_usd,omitempty"`
	TotalCostUSD float64         `json:"total_cost_usd,omitempty"`
}

type ToolUsePayload struct {
	CallID string `json:"call_id"` // tool_call_id assigned by the LLM
	Name   string `json:"name"`
	Args   string `json:"args"`
}

type ToolUseResultPayload struct {
	CallID string `json:"call_id,omitempty"` // tool_call_id linking back to the PreToolUse event
	Name   string `json:"name,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// SubagentPayload is attached to EventSubagentStart and EventSubagentStop.
// Async and TaskID are set on the async path (SpawnBackground) — what
// EventTaskCompleted used to report as a separate event kind — and left zero
// on the synchronous RunSubagent path.
type SubagentPayload struct {
	SessionID    string       `json:"session_id"`
	Prompt       string       `json:"prompt"`
	Output       string       `json:"output,omitempty"`
	UsagePayload UsagePayload `json:"usage,omitempty"`
	Async        bool         `json:"async,omitempty"`
	TaskID       string       `json:"task_id,omitempty"`
}

type CompactPayload struct {
	MessageCount int `json:"message_count"`
	TokenCount   int `json:"token_count"`
}

// CompactSummaryPayload is attached to EventPostCompact. It carries the
// LLM-generated summary plus the before/after shape of the history, so consumers
// can see exactly what compaction collapsed (messages and tokens removed).
type CompactSummaryPayload struct {
	Summary        string `json:"summary"`
	MessagesBefore int    `json:"messages_before,omitempty"`
	MessagesAfter  int    `json:"messages_after,omitempty"`
	TokensBefore   int    `json:"tokens_before,omitempty"`
}

type StopPayload struct {
	Reason string `json:"reason"`
}

// GuardTriggeredPayload is attached to EventGuardTriggered — the one event
// covering every loop guard: the inspection, validation, and completion
// nudges, the repeat-call loop guard, and MaxIter. Name identifies which
// guard fired; Reason is a short human-readable explanation.
type GuardTriggeredPayload struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// UsagePayload is attached to EventStop and contains the total token usage
// across the session and all subagents it spawned, keyed by session ID.
type UsagePayload struct {
	Tokens map[string]Usage `json:"tokens"` // sessionID -> token breakdown
}

// QueueInterruptPayload is attached to EventQueueInterrupt.
// Messages contains the injected messages that caused the stream restart.
type QueueInterruptPayload struct {
	Messages []string `json:"messages"`
}
