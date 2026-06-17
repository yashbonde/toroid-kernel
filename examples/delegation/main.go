// Pattern: DELEGATION & OBSERVABILITY — subagents, background agents, and OTEL.
//
// Delegation has two rungs, both of which the model drives by calling a tool:
//
//   - SUBAGENT (synchronous): the built-in `subagent` tool delegates a
//     self-contained subtask to a fresh child kernel that runs to completion and
//     returns its output. The child inherits the parent's TraceID and is recorded
//     as a child span, so the whole run is one trace. You can also call it from Go
//     with kernel.RunSubagent. Observe with EventSubagentStart/Stop.
//
//   - BACKGROUND (asynchronous): SpawnBackground runs a subtask and returns a
//     task id immediately. When it finishes, its result is queued and the (idle)
//     kernel is woken to process it. The model can trigger the same thing via the
//     `subagent_async` tool. Observe with EventTaskCompleted/EventMasterIdle.
//
// Because this kernel runs with Save:true, every span/cost/event lands in the
// SQLite store (~/.swarmbuddy/sql.db). toroid.OTELSpans(traceID) then exports the
// real run as spec-valid OpenTelemetry spans — the root run and its subagent show
// up as a parent/child span pair, ready for any OTLP backend.
//
//	export ANTHROPIC_API_KEY=your_api_key
//	go run ./examples/delegation
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Println("set ANTHROPIC_API_KEY to run this example")
		return
	}
	ctx := context.Background()

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:   "anthropic/claude-haiku-4-5",
		APIKey:  apiKey,
		WorkDir: ".",
		Save:    true, // persist the trace so we can export it as OTEL below
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	// Surface every tool call the model makes — this is where the `subagent`
	// delegation shows up as an actual tool invocation.
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			fmt.Printf("→ tool %s args=%s\n", p.Name, p.Args)
		}
		return nil
	})
	k.On(toroid.EventSubagentStart, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok {
			fmt.Printf("[subagent started] %s\n", p.Prompt)
		}
		return nil
	})

	// --- SYNCHRONOUS delegation: the model calls the `subagent` tool. ---
	fmt.Println("== subagent (synchronous) ==")
	out, _, err := k.Run(ctx,
		"Use a subagent to read the first 10 lines of go.mod, then summarise the module's dependencies.")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)

	// --- ASYNCHRONOUS delegation: fire-and-wake. ---
	fmt.Println("\n== background (asynchronous) ==")
	done := make(chan struct{})
	k.On(toroid.EventTaskCompleted, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TaskPayload); ok {
			fmt.Printf("[background %s] %s\n", p.TaskID, p.Status)
		}
		return nil
	})
	// MasterIdle fires after the woken turn finishes processing the result.
	k.On(toroid.EventMasterIdle, func(_ context.Context, _ toroid.Event) error {
		select {
		case <-done:
		default:
			close(done)
		}
		return nil
	})

	id := k.SpawnBackground("Count the number of .go files in the working directory.")
	fmt.Printf("spawned background task %s; doing other work…\n", id)
	<-done // the completed task wakes the idle kernel; wait for that turn to end
	fmt.Println("background work absorbed by the main kernel")

	// --- OBSERVABILITY: export the persisted trace as OpenTelemetry spans. ---
	fmt.Println("\n== OTEL export ==")
	traceID := k.SessionID() // root kernel: SessionID == TraceID
	spans, err := toroid.OTELSpans(traceID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("OTEL spans for trace %s:\n", traceID)
	for _, s := range spans {
		fmt.Printf("  span=%s parent=%s name=%-9s cost=$%.4f\n",
			s.SpanID, s.ParentSpanID, s.Name, s.CostUSD)
	}

	sessions, err := toroid.ListSessions()
	if err != nil {
		panic(err)
	}
	fmt.Printf("\n%d session(s) in the store (persisted at ~/.swarmbuddy/sql.db)\n", len(sessions))
}
