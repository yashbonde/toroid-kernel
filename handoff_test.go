package toroid

import (
	"testing"

	"charm.land/fantasy"
)

func hasReasoning(msg fantasy.Message) bool {
	for _, p := range msg.Content {
		if _, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](p); ok {
			return true
		}
	}
	return false
}

func hasText(msg fantasy.Message, want string) bool {
	for _, p := range msg.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && tp.Text == want {
			return true
		}
	}
	return false
}

// TestHandoffSameAPIPassthrough verifies that the in-scope path (both models on
// openai-completions) leaves history untouched (M9).
func TestHandoffSameAPIPassthrough(t *testing.T) {
	from := ResolveModel("llmgateway/claude-sonnet-4-5")
	to := ResolveModel("llmgateway/claude-haiku-4-5") // both openai-completions
	if from.API != APIOpenAICompletions || to.API != APIOpenAICompletions {
		t.Fatalf("expected both models on openai-completions, got %q/%q", from.API, to.API)
	}

	history := []fantasy.Message{{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "thinking"}, fantasy.TextPart{Text: "answer"}},
	}}
	out := TransformForHandoff(history, from, to)
	if !hasReasoning(out[0]) {
		t.Error("same-API handoff must preserve reasoning blocks unchanged")
	}
}

// TestHandoffCrossAPIStripsReasoning verifies that a cross-API handoff drops
// reasoning blocks but preserves text, tool calls, and results (M9).
func TestHandoffCrossAPIStripsReasoning(t *testing.T) {
	from := ResolveModel("anthropic/claude-opus-4.8")  // anthropic-messages
	to := ResolveModel("llmgateway/claude-sonnet-4-5") // openai-completions
	if from.API == to.API {
		t.Fatal("test needs differing APIs")
	}

	history := []fantasy.Message{{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "private thoughts"},
			fantasy.TextPart{Text: "visible answer"},
		},
	}}
	out := TransformForHandoff(history, from, to)
	if hasReasoning(out[0]) {
		t.Error("cross-API handoff must strip reasoning blocks")
	}
	if !hasText(out[0], "visible answer") {
		t.Error("cross-API handoff must preserve text content")
	}
}
