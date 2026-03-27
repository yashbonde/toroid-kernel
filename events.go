package toroid

import "encoding/json"

type EventKind string

const (
	EventTraceLog           EventKind = "TraceLog" // structured log entry stored in the trace; visible in UI and readable by follow-on agents
	EventSessionStart       EventKind = "SessionStart"
	EventUserPromptSubmit   EventKind = "UserPromptSubmit"
	EventToken              EventKind = "Token"              // each streamed text chunk (display only, not stored)
	EventPermissionRequest  EventKind = "PermissionRequest"  // before a tool is called, if permission is required
	EventPreToolUse         EventKind = "PreToolUse"         // before a tool is called
	EventPostToolUse        EventKind = "PostToolUse"        // after a tool call is completed
	EventPostToolUseFailure EventKind = "PostToolUseFailure" // after a tool call fails
	EventSubagentStart      EventKind = "SubagentStart"      // before the subagent is started
	EventSubagentStop       EventKind = "SubagentStop"       // before the subagent is stopped
	EventMasterIdle         EventKind = "MasterIdle"         // after the main agent is idle
	EventNotification       EventKind = "Notification"       // before the notification is sent
	EventTaskCompleted      EventKind = "TaskCompleted"      // before the task is completed
	EventTitle              EventKind = "Title"              // fired async when session title is ready
	EventReasoning          EventKind = "Reasoning"          // streamed reasoning/thinking tokens (display only, not stored)
	EventAssistantTurn      EventKind = "AssistantTurn"      // full structured content blocks for the turn (thinking+text+tool_use)
	EventTurnCost           EventKind = "TurnCost"           // after each LLM turn, with incremental cost
	EventStop               EventKind = "Stop"               // when the agent is stopped
	EventPreCompact         EventKind = "PreCompact"         // before compacting the memory
	EventPostCompact        EventKind = "PostCompact"        // after compaction; payload contains the LLM-generated summary
	EventSessionEnd         EventKind = "SessionEnd"         // after the session ends
)

type Event struct {
	Kind      EventKind `json:"kind"`
	SessionID string    `json:"session_id"`
	TraceID   string    `json:"trace_id"`
	SpanID    string    `json:"span_id"`
	EmitTS    int64     `json:"emit_ts"` // UnixNano wall clock
	Seq       uint64    `json:"seq"`     // monotonic counter within a span
	Payload   any       `json:"payload,omitempty"`
}

// fantasy event to Swarm Buddy event Map

const (
	TraceLogInfo    = "info"
	TraceLogWarning = "warning"
	TraceLogError   = "error"
)

// TraceLogPayload is attached to EventTraceLog.
type TraceLogPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Payload types

type UserPromptPayload struct {
	Prompt string `json:"prompt"`
}

type TokenPayload struct {
	Text string `json:"text"`
}

type ReasoningPayload struct {
	Text string `json:"text"`
}

type TitlePayload struct {
	Title string `json:"title"`
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

type SubagentPayload struct {
	SessionID    string       `json:"session_id"`
	Prompt       string       `json:"prompt"`
	Output       string       `json:"output,omitempty"`
	UsagePayload UsagePayload `json:"usage,omitempty"`
}

type NotificationPayload struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

type CompactPayload struct {
	MessageCount int `json:"message_count"`
	TokenCount   int `json:"token_count"`
}

// CompactSummaryPayload is attached to EventPostCompact.
// It contains the LLM-generated summary of the conversation before the context was reset.
type CompactSummaryPayload struct {
	Summary string `json:"summary"`
}

// AssistantTurnPayload is attached to EventAssistantTurn.
// Messages is a JSON-serialized []fantasy.Message containing the full structured
// content blocks (thinking, text, tool_use, tool_result) from all steps of the turn.
type AssistantTurnPayload struct {
	Messages json.RawMessage `json:"messages"`
}

type StopPayload struct {
	Reason string `json:"reason"`
}

type PermissionPayload struct {
	ToolName string         `json:"tool_name"`
	Args     map[string]any `json:"args"`
	Verdict  string         `json:"verdict"` // "allow" | "deny"
}

type TaskPayload struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// UsagePayload is attached to EventStop and contains the total token usage
// across the session and all subagents it spawned, keyed by session ID.
type UsagePayload struct {
	Tokens map[string]Usage `json:"tokens"` // sessionID -> token breakdown
}

// TurnCostPayload is attached to EventTurnCost, fired after each LLM turn.
type TurnCostPayload struct {
	TurnUsage    Usage   `json:"turn_usage"`     // tokens consumed in this single turn
	TurnCostUSD  float64 `json:"turn_cost_usd"`  // cost of this turn in USD
	TotalCostUSD float64 `json:"total_cost_usd"` // cumulative cost so far in USD
}
