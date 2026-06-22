// Pattern: LOOP GUARDRAILS — stopping runaway agent loops, with a MOCK LLM.
//
// The agent loop keeps calling tools until the model stops asking for them. Two
// things can go wrong:
//
//   - MaxIter: an absolute cap on tool-call steps (wired via fantasy's
//     StepCountIs). Without it the loop is unbounded.
//
//   - MaxRepeatCalls: a "spin" guard. If the model issues the SAME tool call
//     (name + input) and gets the SAME result N steps in a row, it is making no
//     progress and the loop is stopped early. Crucially the guard keys on the
//     RESULT too, so legitimate polling — same args, but a result that changes
//     over time — is never killed.
//
// This example needs NO API key and makes NO network calls: it drives the kernel
// with a mock fantasy.LanguageModel so the guard behaviour is deterministic.
//
//	go run ./examples/guardrails
package main

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	toroid "github.com/yashbonde/toroid-kernel"
	tools "github.com/yashbonde/toroid-kernel/tools"
)

// --- mock LLM plumbing -----------------------------------------------------

// mockProvider hands the kernel our scripted language model instead of a real
// provider. Supplying Config.Provider bypasses the API-key path entirely.
type mockProvider struct{ lm fantasy.LanguageModel }

func (p mockProvider) Name() string { return "mock" }
func (p mockProvider) LanguageModel(_ context.Context, _ string) (fantasy.LanguageModel, error) {
	return p.lm, nil
}

// mockLLM streams whatever its stream func returns. We only implement Stream
// because the kernel's run loop is streaming-only.
type mockLLM struct {
	stream func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error)
}

func (m *mockLLM) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	return m.stream(ctx, call)
}
func (m *mockLLM) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, fmt.Errorf("not used")
}
func (m *mockLLM) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, fmt.Errorf("not used")
}
func (m *mockLLM) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, fmt.Errorf("not used")
}
func (m *mockLLM) Provider() string { return "mock" }
func (m *mockLLM) Model() string    { return "mock-model" }

// emitToolCall yields a single tool call followed by a tool-calls finish, which
// tells the agent loop to run the tool and come back for another step.
func emitToolCall(name, input string) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{
			Type:          fantasy.StreamPartTypeToolCall,
			ID:            "call",
			ToolCallName:  name,
			ToolCallInput: input,
		}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonToolCalls,
			Usage:        fantasy.Usage{TotalTokens: 5},
		})
	}
}

// emitText yields a final text answer and a normal stop, ending the loop.
func emitText(text string) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t"}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{TotalTokens: 5},
		})
	}
}

// newKernel builds a kernel wired to a mock LLM and a single custom tool. We
// disable the built-in computer tools so only our tool is in play.
func newKernel(ctx context.Context, cfg toroid.Config, lm fantasy.LanguageModel, tool *tools.ToolDef) *toroid.Kernel {
	cfg.Provider = mockProvider{lm: lm}
	cfg.Model = "mock/mock-model"
	cfg.ComputerTools = false
	// The agent's tool list is frozen when NewKernel builds it, so custom tools
	// must be supplied via Config.Tools (merged in before that) rather than
	// registered afterwards.
	reg := tools.NewRegistry()
	reg.Register(tool)
	cfg.Tools = reg
	k, err := toroid.NewKernel(ctx, cfg)
	if err != nil {
		panic(err)
	}
	return k
}

func main() {
	ctx := context.Background()

	// ---------------------------------------------------------------------
	// Scenario 1: a STUCK loop. The model calls ping({}) forever and the tool
	// always returns "pong". Identical call + identical result => the repeat
	// guard trips. We set MaxRepeatCalls=3 and MaxIter=50, so the guard fires
	// long before MaxIter, proving it is the thing stopping the loop.
	// ---------------------------------------------------------------------
	pingCalls := 0
	pingTool := &tools.ToolDef{
		Name:        "ping",
		Description: "always returns pong",
		AgentTool: fantasy.NewAgentTool("ping", "always returns pong",
			func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				pingCalls++
				return fantasy.ToolResponse{Type: "text", Content: "pong"}, nil
			}),
	}
	stuckLLM := &mockLLM{stream: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		return emitToolCall("ping", "{}"), nil // identical every time
	}}
	k1 := newKernel(ctx, toroid.Config{MaxIter: 50, MaxRepeatCalls: 3}, stuckLLM, pingTool)
	defer k1.Close()

	fmt.Println("== scenario 1: stuck loop (identical call + identical result) ==")
	if _, _, err := k1.Run(ctx, "ping repeatedly"); err != nil {
		panic(err)
	}
	fmt.Printf("ping tool ran %d times (guard MaxRepeatCalls=3, MaxIter=50)\n", pingCalls)
	if pingCalls == 3 {
		fmt.Print("PASS: repeat guard stopped the spin at 3, well before MaxIter\n\n")
	} else {
		fmt.Printf("FAIL: expected 3, got %d\n\n", pingCalls)
	}

	// ---------------------------------------------------------------------
	// Scenario 2: legitimate POLLING. The model calls poll({}) with the SAME
	// args each time, but the tool reports changing state ("pending 1",
	// "pending 2", ... "done"). Because the guard keys on the result, the
	// changing output means it never trips — the loop runs until the model
	// itself stops once it sees "done". Same MaxRepeatCalls=3 as scenario 1.
	// ---------------------------------------------------------------------
	pollCalls := 0
	pollTool := &tools.ToolDef{
		Name:        "poll",
		Description: "polls a job; result changes over time",
		AgentTool: fantasy.NewAgentTool("poll", "polls a job; result changes over time",
			func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				pollCalls++
				if pollCalls >= 4 {
					return fantasy.ToolResponse{Type: "text", Content: "status: done"}, nil
				}
				return fantasy.ToolResponse{Type: "text", Content: fmt.Sprintf("status: pending %d", pollCalls)}, nil
			}),
	}
	pollLLM := &mockLLM{stream: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		// Keep polling until a tool result in the history says "done".
		if promptContains(call, "status: done") {
			return emitText("job finished"), nil
		}
		return emitToolCall("poll", "{}"), nil // identical args every time
	}}
	k2 := newKernel(ctx, toroid.Config{MaxIter: 50, MaxRepeatCalls: 3}, pollLLM, pollTool)
	defer k2.Close()

	fmt.Println("== scenario 2: polling (identical args, changing result) ==")
	out, _, err := k2.Run(ctx, "poll the job until done")
	if err != nil {
		panic(err)
	}
	fmt.Printf("poll tool ran %d times, final answer: %q\n", pollCalls, out)
	if pollCalls == 4 {
		fmt.Println("PASS: guard left the progressing poll alone; it ran to completion")
	} else {
		fmt.Printf("FAIL: expected 4 poll calls, got %d\n", pollCalls)
	}
}

// promptContains reports whether any text part of the prompt contains s. Used to
// let the mock model react to the latest tool result.
func promptContains(call fantasy.Call, s string) bool {
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if txt, ok := tr.Output.(fantasy.ToolResultOutputContentText); ok {
					if strings.Contains(txt.Text, s) {
						return true
					}
				}
			}
		}
	}
	return false
}
