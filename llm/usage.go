package llm

// Usage is the token accounting for one llm-step, as reported by the gateway.
// It holds token counts only — dollar cost is computed a layer up (toroid's
// pricing table) or taken from the gateway's authoritative cost header, so this
// package stays free of any pricing policy.
type Usage struct {
	Input      int64 // fresh (non-cached) prompt tokens
	Output     int64 // completion tokens (includes Reasoning)
	Reasoning  int64 // reasoning/thinking tokens (subset of Output)
	CacheRead  int64 // prompt tokens served from cache
	CacheWrite int64 // prompt tokens written to cache
}

// FinishReason is why the model stopped, normalized from the OpenAI wire.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"       // produced a final answer
	FinishToolCalls FinishReason = "tool_calls" // wants tools run
	FinishLength    FinishReason = "length"     // hit max output tokens
	FinishError     FinishReason = "error"      // provider/gateway error
	FinishOther     FinishReason = "other"      // content filter / unknown
)

// normalizeFinishReason maps an OpenAI finish_reason string to a FinishReason.
func normalizeFinishReason(s string) FinishReason {
	switch s {
	case "stop", "end_turn":
		return FinishStop
	case "tool_calls", "tool_use", "function_call":
		return FinishToolCalls
	case "length", "max_tokens":
		return FinishLength
	case "content_filter":
		return FinishOther
	case "":
		return FinishOther
	default:
		return FinishOther
	}
}
