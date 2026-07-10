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
