package toroid

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/yashbonde/toroid-kernel/tools"
)

// registerEchoTool adds a trivial tool named "echo" that returns its input and
// counts invocations, so the kernel-owned loop's tool execution is observable.
func registerEchoTool(reg *tools.Registry, calls *int32) {
	type echoIn struct {
		Msg string `json:"msg"`
	}
	fTool := fantasy.NewAgentTool("echo", "echo the message",
		func(ctx context.Context, in echoIn, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			atomic.AddInt32(calls, 1)
			return fantasy.NewTextResponse("echoed: " + in.Msg), nil
		})
	reg.Register(&tools.ToolDef{Name: "echo", Description: "echo", AgentTool: fTool})
}

// TestStepLoopExecutesToolsAndBills drives the kernel-owned Step loop end-to-end:
// the model asks for a tool twice, then answers. The kernel must run the tool,
// append results to history, fire tool events, bill every llm-step, and write the
// final answer (P1.1).
func TestStepLoopExecutesToolsAndBills(t *testing.T) {
	var toolCalls int32
	reg := tools.NewRegistry()
	registerEchoTool(reg, &toolCalls)

	faux := &FauxStep{Replies: []FauxReply{
		{ToolCalls: []FauxToolCall{{ID: "c1", Name: "echo", Input: `{"msg":"a"}`}}, Usage: fantasy.Usage{InputTokens: 1000, OutputTokens: 20}},
		{ToolCalls: []FauxToolCall{{ID: "c2", Name: "echo", Input: `{"msg":"b"}`}}, Usage: fantasy.Usage{InputTokens: 1100, OutputTokens: 20}},
		{Text: "all done", Usage: fantasy.Usage{InputTokens: 1200, OutputTokens: 30}},
	}}

	k := newTestKernel(faux)
	k.Tools = reg
	k.Cfg.UseStepLoop = true
	k.Cfg.MaxIter = 25
	k.History = []fantasy.Message{fantasy.NewUserMessage("do the echo dance")}

	var pre, post int
	k.On(EventPreToolUse, func(context.Context, Event) error { pre++; return nil })
	k.On(EventPostToolUse, func(context.Context, Event) error { post++; return nil })

	var out strings.Builder
	if err := k.streamViaStep(context.Background(), &out); err != nil {
		t.Fatal(err)
	}

	if toolCalls != 2 {
		t.Errorf("echo tool ran %d times, want 2", toolCalls)
	}
	if pre != 2 || post != 2 {
		t.Errorf("tool events pre=%d post=%d, want 2/2", pre, post)
	}
	if out.String() != "all done" {
		t.Errorf("final answer = %q, want %q", out.String(), "all done")
	}
	if k.RunningCostUSD() <= 0 {
		t.Errorf("expected all 3 llm-steps billed, cost=%v", k.RunningCostUSD())
	}
	// History: user + (assistant toolcall + tool result)*2 + final assistant = 6.
	if len(k.History) != 6 {
		t.Errorf("history len = %d, want 6", len(k.History))
	}
	// A tool-result message must be present and carry the echoed output.
	foundResult := false
	for _, m := range k.History {
		if m.Role == fantasy.MessageRoleTool {
			for _, p := range m.Content {
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok {
					if txt, ok := tr.Output.(fantasy.ToolResultOutputContentText); ok && strings.Contains(txt.Text, "echoed:") {
						foundResult = true
					}
				}
			}
		}
	}
	if !foundResult {
		t.Error("expected an echoed tool-result message in history")
	}
}

// TestStepLoopMaxIter verifies the loop honours MaxIter when the model never
// stops asking for tools (P1.1 guard).
func TestStepLoopMaxIter(t *testing.T) {
	var toolCalls int32
	reg := tools.NewRegistry()
	registerEchoTool(reg, &toolCalls)

	// Always asks for a tool; only the loop cap can stop it. Vary input so the
	// repeat-call guard does not trip first.
	faux := &FauxStep{Replies: []FauxReply{
		{ToolCalls: []FauxToolCall{{ID: "a", Name: "echo", Input: `{"msg":"1"}`}}, Usage: fantasy.Usage{InputTokens: 10}},
		{ToolCalls: []FauxToolCall{{ID: "b", Name: "echo", Input: `{"msg":"2"}`}}, Usage: fantasy.Usage{InputTokens: 10}},
		{ToolCalls: []FauxToolCall{{ID: "c", Name: "echo", Input: `{"msg":"3"}`}}, Usage: fantasy.Usage{InputTokens: 10}},
		{ToolCalls: []FauxToolCall{{ID: "d", Name: "echo", Input: `{"msg":"4"}`}}, Usage: fantasy.Usage{InputTokens: 10}},
		{ToolCalls: []FauxToolCall{{ID: "e", Name: "echo", Input: `{"msg":"5"}`}}, Usage: fantasy.Usage{InputTokens: 10}},
	}}

	k := newTestKernel(faux)
	k.Tools = reg
	k.Cfg.UseStepLoop = true
	k.Cfg.MaxIter = 3
	k.Cfg.MaxRepeatCalls = 0 // disable repeat guard for this test
	k.History = []fantasy.Message{fantasy.NewUserMessage("loop forever")}

	if err := k.streamViaStep(context.Background(), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if toolCalls != 3 {
		t.Errorf("expected MaxIter=3 tool turns, ran %d", toolCalls)
	}
}

// TestStepLoopRepeatGuard verifies the loop stops when the model spins on the
// identical tool call with identical result (P1.1 loop guard).
func TestStepLoopRepeatGuard(t *testing.T) {
	var toolCalls int32
	reg := tools.NewRegistry()
	registerEchoTool(reg, &toolCalls)

	same := FauxReply{ToolCalls: []FauxToolCall{{ID: "x", Name: "echo", Input: `{"msg":"same"}`}}, Usage: fantasy.Usage{InputTokens: 10}}
	faux := &FauxStep{Replies: []FauxReply{same, same, same, same, same}}

	k := newTestKernel(faux)
	k.Tools = reg
	k.Cfg.UseStepLoop = true
	k.Cfg.MaxIter = 25
	k.Cfg.MaxRepeatCalls = 3
	k.History = []fantasy.Message{fantasy.NewUserMessage("spin")}

	if err := k.streamViaStep(context.Background(), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	// Stops at 3 identical consecutive calls, not 5.
	if toolCalls != 3 {
		t.Errorf("repeat guard should stop at 3 identical calls, ran %d", toolCalls)
	}
}
