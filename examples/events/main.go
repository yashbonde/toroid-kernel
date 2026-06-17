// Pattern: EVENTS & HOOKS (observability).
//
// The kernel exposes its whole lifecycle through a synchronous event bus. Use
// On(kind, fn) to observe tool calls, costs, reasoning, compaction, etc. — this
// is how a host renders output, tracks spend, and reacts to lifecycle changes.
// A hook returning a non-nil error aborts the firing chain.
//
//	export GEMINI_TOKEN=your_api_key
//	go run ./examples/events
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	apiKey := os.Getenv("GEMINI_TOKEN")
	if apiKey == "" {
		fmt.Println("set GEMINI_TOKEN to run this example")
		return
	}
	ctx := context.Background()

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:   "google/gemini-3-flash-preview",
		APIKey:  apiKey,
		WorkDir: ".",
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			fmt.Printf("→ tool %s args=%s\n", p.Name, p.Args)
		}
		return nil
	})
	k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			fmt.Printf("← tool %s done\n", p.Name)
		}
		return nil
	})
	k.On(toroid.EventTurnCost, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TurnCostPayload); ok {
			fmt.Printf("$ turn=$%.6f total=$%.6f\n", p.TurnCostUSD, p.TotalCostUSD)
		}
		return nil
	})

	out, _, err := k.Run(ctx, "List the files here, then tell me how many there are.")
	if err != nil {
		panic(err)
	}
	fmt.Println("\n" + out)
}
