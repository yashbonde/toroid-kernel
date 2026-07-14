package toroid

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// TestKernelE2E is the one end-to-end test: it drives a real Kernel (built via
// NewKernel) through the kernel-owned step loop on a scripted FauxStep — no
// network. One run covers the critical pipeline:
//
//	tool loop (model asks for a tool → kernel runs it → final answer)
//	per-step cost accounting (every llm-step billed with gateway-reported cost)
//	history shape + AssistantTurn events parsing back as []llm.Message
//	(what resume and the OTEL/Langfuse exporter consume)
//	structured output (WithSchema object pass, billed)
//
// Anything deeper is verified live against the gateway (see examples/).
func TestKernelE2E(t *testing.T) {
	ctx := context.Background()

	toolRan := 0
	reg := tools.NewRegistry()
	reg.Register(&tools.ToolDef{
		Name: "echo", Description: "echo the message",
		Handler: llm.NewTool("echo", "echo the message",
			func(ctx context.Context, in struct {
				Msg string `json:"msg"`
			}) (llm.ToolResult, error) {
				toolRan++
				return llm.NewTextResult("echoed: " + in.Msg), nil
			}),
	})

	k, err := NewKernel(ctx, Config{
		Model:                "llmgateway/claude-haiku-4-5", // priced in the catalog
		WorkDir:              t.TempDir(),
		IncludeComputerTools: false,
		Tools:                reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()

	// Scripted model: one tool turn, then the final answer; then the object pass.
	k.Step = &FauxStep{
		Replies: []FauxReply{
			{ToolCalls: []FauxToolCall{{ID: "c1", Name: "echo", Input: `{"msg":"hi"}`}}, Usage: llm.Usage{Input: 1000, Output: 20}, Cost: 0.002},
			{Text: "all done", Usage: llm.Usage{Input: 1100, Output: 30}, Cost: 0.003},
		},
		Object: FauxObject{Object: map[string]any{"ok": true}, Usage: llm.Usage{Input: 500, Output: 10}, Cost: 0.001},
	}

	// Capture the events the store/exporter/resume path depends on.
	var turnPayloads []json.RawMessage
	k.On(EventAssistantTurn, func(_ context.Context, e Event) error {
		turnPayloads = append(turnPayloads, e.Payload.(*AssistantTurnPayload).Messages)
		return nil
	})

	out, usage, err := k.Run(ctx, "do the echo dance")
	if err != nil {
		t.Fatal(err)
	}

	if out != "all done" {
		t.Errorf("final answer = %q, want %q", out, "all done")
	}
	if toolRan != 1 {
		t.Errorf("echo tool ran %d times, want 1", toolRan)
	}
	// History: user + assistant(tool call) + tool result + final assistant.
	if len(k.History) != 4 {
		t.Errorf("history len = %d, want 4", len(k.History))
	}
	// Both llm-steps billed and priced (PricingOK, positive cost).
	if k.RunningCostUSD() <= 0 {
		t.Errorf("expected billed llm-steps, cost = %v", k.RunningCostUSD())
	}
	if len(usage.Tokens) == 0 {
		t.Error("EventStop usage payload empty")
	}
	// Every persisted assistant turn must round-trip as []llm.Message — this is
	// the contract resume (ReconstructHistory) and the OTEL exporter rely on.
	if len(turnPayloads) < 2 {
		t.Fatalf("expected >=2 AssistantTurn events, got %d", len(turnPayloads))
	}
	var sawToolCall bool
	for _, raw := range turnPayloads {
		var msgs []llm.Message
		if err := json.Unmarshal(raw, &msgs); err != nil {
			t.Fatalf("AssistantTurn payload does not parse as []llm.Message: %v", err)
		}
		for _, m := range msgs {
			if len(m.ToolCalls()) > 0 {
				sawToolCall = true
			}
		}
	}
	if !sawToolCall {
		t.Error("no persisted turn carried the tool call")
	}

	// Structured output: the object pass is one more billed llm-step.
	costBefore := k.RunningCostUSD()
	var buf strings.Builder
	err = k.Stream(ctx, "return it as JSON", &buf, WithSchema(
		Schema{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
		"result", "the result"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"ok":true`) {
		t.Errorf("schema output = %q", buf.String())
	}
	if k.RunningCostUSD() <= costBefore {
		t.Error("object llm-step was not billed")
	}
}

func TestSpendLimitsE2E(t *testing.T) {
	newKernel := func(t *testing.T, transcriptMax float64, replies []FauxReply) (*Kernel, *FauxStep, *int) {
		t.Helper()
		toolRuns := 0
		reg := tools.NewRegistry()
		reg.Register(&tools.ToolDef{
			Name: "echo", Description: "echo the message",
			Handler: llm.NewTool("echo", "echo the message",
				func(context.Context, struct {
					Msg string `json:"msg"`
				}) (llm.ToolResult, error) {
					toolRuns++
					return llm.NewTextResult("ok"), nil
				}),
		})
		k, err := NewKernel(context.Background(), Config{
			Model:                 "llmgateway/claude-haiku-4-5",
			WorkDir:               t.TempDir(),
			IncludeComputerTools:  false,
			Tools:                 reg,
			MaxTranscriptSpendUSD: transcriptMax,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { k.Close() })
		step := &FauxStep{
			Replies: replies,
			Object:  FauxObject{Object: map[string]any{"ok": true}, Cost: 0.01},
		}
		k.Step = step
		return k, step, &toolRuns
	}

	toolReply := func(id string, cost float64) FauxReply {
		return FauxReply{
			ToolCalls: []FauxToolCall{{ID: id, Name: "echo", Input: `{"msg":"x"}`}},
			Cost:      cost,
		}
	}

	t.Run("turn limit stops tools and structured output", func(t *testing.T) {
		k, step, toolRuns := newKernel(t, 0, []FauxReply{
			toolReply("c1", 0.01),
			toolReply("c2", 0.01),
			toolReply("c3", 0.01),
			{Text: "too late", Cost: 0.01},
		})
		var out strings.Builder
		err := k.Stream(context.Background(), "work", &out,
			WithMaxTurnSpendUSD(0.025),
			WithSchema(Schema{"type": "object"}, "result", "result"))
		if err != nil {
			t.Fatal(err)
		}
		if step.next != 3 || *toolRuns != 2 {
			t.Fatalf("llm steps=%d tool runs=%d, want 3 and 2", step.next, *toolRuns)
		}
		if out.Len() != 0 {
			t.Fatalf("structured-output call ran after limit: %q", out.String())
		}
	})

	t.Run("transcript limit blocks the next call", func(t *testing.T) {
		k, step, toolRuns := newKernel(t, 0.025, []FauxReply{
			toolReply("c1", 0.01),
			{Text: "first done", Cost: 0.01},
			toolReply("c2", 0.01),
			{Text: "too late", Cost: 0.01},
		})
		if _, _, err := k.Run(context.Background(), "first"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := k.Run(context.Background(), "second"); err != nil {
			t.Fatal(err)
		}
		stepsAtLimit := step.next
		if _, _, err := k.Run(context.Background(), "third"); err != nil {
			t.Fatal(err)
		}
		if step.next != stepsAtLimit || step.next != 3 || *toolRuns != 1 {
			t.Fatalf("llm steps=%d tool runs=%d, want 3 and 1 with no third-call llm-step", step.next, *toolRuns)
		}
	})
}
