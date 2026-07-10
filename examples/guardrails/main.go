// Pattern: LOOP GUARDRAILS — stopping runaway agent loops, with a SCRIPTED LLM.
//
// The agent loop keeps calling tools until the model stops asking for them. Two
// things can go wrong:
//
//   - MaxIter: an absolute cap on tool-call steps. Without it the loop is
//     unbounded.
//
//   - MaxRepeatCalls: a "spin" guard. If the model issues the SAME tool call
//     (name + input) and gets the SAME result N steps in a row, it is making no
//     progress and the loop is stopped early. Crucially the guard keys on the
//     RESULT too, so legitimate polling — same args, but a result that changes
//     over time — is never killed.
//
// This example needs NO API key and makes NO network calls: it drives the kernel
// with a scripted FauxStep so the guard behaviour is deterministic.
//
//	go run ./examples/guardrails
package main

import (
	"context"
	"fmt"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
	tools "github.com/yashbonde/toroid-kernel/tools"
)

// newKernel builds a kernel wired to a scripted Step and a single custom tool.
// We disable the built-in computer tools so only our tool is in play.
func newKernel(ctx context.Context, cfg toroid.Config, step toroid.Step, tool *tools.ToolDef) *toroid.Kernel {
	cfg.Model = "mock/mock-model"
	cfg.IncludeComputerTools = false
	reg := tools.NewRegistry()
	reg.Register(tool)
	cfg.Tools = reg
	k, err := toroid.NewKernel(ctx, cfg)
	if err != nil {
		panic(err)
	}
	k.Step = step // no network: every llm-step comes from the script
	return k
}

// toolCallReply scripts one turn where the model asks for a tool.
func toolCallReply(name, input string) toroid.FauxReply {
	return toroid.FauxReply{
		ToolCalls: []toroid.FauxToolCall{{ID: "call", Name: name, Input: input}},
		Usage:     llm.Usage{Input: 5, Output: 5},
	}
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
		Handler: llm.NewTool("ping", "always returns pong",
			func(ctx context.Context, _ struct{}) (llm.ToolResult, error) {
				pingCalls++
				return llm.NewTextResult("pong"), nil
			}),
	}
	// A single scripted reply repeats forever once the script is exhausted.
	stuck := &toroid.FauxStep{Replies: []toroid.FauxReply{toolCallReply("ping", "{}")}}
	k1 := newKernel(ctx, toroid.Config{MaxIter: 50, MaxRepeatCalls: 3}, stuck, pingTool)
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
	// stops once the job is done. Same MaxRepeatCalls=3 as scenario 1.
	// ---------------------------------------------------------------------
	pollCalls := 0
	pollTool := &tools.ToolDef{
		Name:        "poll",
		Description: "polls a job; result changes over time",
		Handler: llm.NewTool("poll", "polls a job; result changes over time",
			func(ctx context.Context, _ struct{}) (llm.ToolResult, error) {
				pollCalls++
				if pollCalls >= 4 {
					return llm.NewTextResult("status: done"), nil
				}
				return llm.NewTextResult(fmt.Sprintf("status: pending %d", pollCalls)), nil
			}),
	}
	poller := &toroid.FauxStep{Replies: []toroid.FauxReply{
		toolCallReply("poll", "{}"),
		toolCallReply("poll", "{}"),
		toolCallReply("poll", "{}"),
		toolCallReply("poll", "{}"), // 4th call observes "status: done"
		{Text: "job finished", Usage: llm.Usage{Input: 5, Output: 5}},
	}}
	k2 := newKernel(ctx, toroid.Config{MaxIter: 50, MaxRepeatCalls: 3}, poller, pollTool)
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
