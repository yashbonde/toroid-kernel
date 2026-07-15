// Pattern: EVENTS (lifecycle observability).
//
// The kernel exposes its whole lifecycle through a synchronous event bus. Use
// On(kind, fn) to observe tool calls, costs, reasoning, compaction, etc. — this
// is how a host renders output, tracks spend, and reacts to lifecycle changes.
// A hook returning a non-nil error aborts the firing chain.
//
//	export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
//	go run ./examples/events
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	// Prefer llmgateway when configured, fall back to Anthropic.
	apiKey := os.Getenv("LLM_GATEWAY_KEY")
	model := "llmgateway/claude-haiku-4-5"
	if apiKey == "" {
		fmt.Println("set LLM_GATEWAY_KEY to run this example")
		return
	}
	// Allow overriding the model via env to run the example across providers.
	if m := os.Getenv("TOROID_MODEL"); m != "" {
		model = m
	}
	ctx := context.Background()

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                model,
		APIKey:               apiKey,
		WorkDir:              ".",
		IncludeComputerTools: true,
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	// Observe the lifecycle: tool calls, their results, and per-turn cost.
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
	out, _, err := k.Run(ctx, "List the files here and tell me how many there are.")
	if err != nil {
		panic(err)
	}
	fmt.Println("\n" + out)
}
