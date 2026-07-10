package toroid

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// newTestKernel builds a minimal in-memory Kernel (no provider/network) suitable
// for exercising Step-backed paths like Compact with a FauxStep.
func newTestKernel(step Step) *Kernel {
	return &Kernel{
		Cfg:          Config{Model: "llmgateway/claude-sonnet-4-5", TotalContextSize: 200000},
		Hooks:        &HookRegistry{},
		Sessions:     map[string]Usage{},
		Step:         step,
		SystemPrompt: "you are a helpful agent",
	}
}

// TestCompactRunsThroughStepAndBills verifies P1.2: the compaction summarize call
// goes through the Step layer, is billed into running cost, resets history to the
// summary, and fires the host OnPayload debug hook (P2.2).
func TestCompactRunsThroughStepAndBills(t *testing.T) {
	faux := &FauxStep{Replies: []FauxReply{{
		Text:  "SUMMARY: user said hello and we discussed compaction.",
		Usage: fantasy.Usage{InputTokens: 2000, OutputTokens: 120},
	}}}
	k := newTestKernel(faux)

	var payloads []PayloadDebug
	k.Cfg.OnPayload = func(p PayloadDebug) { payloads = append(payloads, p) }

	k.History = []fantasy.Message{
		fantasy.NewUserMessage("hello"),
		fantasy.NewUserMessage("more context here"),
	}

	if err := k.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}

	// History collapses to two messages: the summary prompt + assistant summary.
	if len(k.History) != 2 {
		t.Fatalf("expected 2 messages after compact, got %d", len(k.History))
	}
	if k.History[1].Role != fantasy.MessageRoleAssistant {
		t.Errorf("summary message role = %q, want assistant", k.History[1].Role)
	}
	if got := messageText(k.History[1]); !strings.Contains(got, "SUMMARY") {
		t.Errorf("summary not carried into history: %q", got)
	}

	// The compact llm-step must be billed (previously invisible).
	if k.RunningCostUSD() <= 0 {
		t.Errorf("expected compact to bill running cost, got %v", k.RunningCostUSD())
	}

	// OnPayload must have fired for the compact step and carried the single system.
	if len(payloads) != 1 {
		t.Fatalf("expected 1 OnPayload call, got %d", len(payloads))
	}
	if payloads[0].System != k.SystemPrompt {
		t.Errorf("payload system = %q, want %q", payloads[0].System, k.SystemPrompt)
	}
}

func messageText(m fantasy.Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}
