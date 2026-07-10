// Pattern: EVENTS & NOTIFICATIONS (observability + pluggable sinks).
//
// The kernel exposes its whole lifecycle through a synchronous event bus. Use
// On(kind, fn) to observe tool calls, costs, reasoning, compaction, etc. — this
// is how a host renders output, tracks spend, and reacts to lifecycle changes.
// A hook returning a non-nil error aborts the firing chain.
//
// Notifications are a specialised case: the `notify` tool fires an
// EventNotification on the bus AND runs any sinks registered with
// tools.RegisterNotifySink (plus a best-effort desktop notification). Register
// your own sink to route notifications to a webhook, Slack, or a peer kernel —
// the tool itself stays platform-agnostic.
//
//	export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
//	go run ./examples/events
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/tools"
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

	// Register a custom notification sink BEFORE constructing the kernel. In a
	// real host this might POST to a webhook or forward to a peer kernel.
	tools.RegisterNotifySink(func(_ context.Context, title, message string) error {
		fmt.Printf("[sink] %s: %s\n", title, message)
		return nil
	})

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
	// Notifications also arrive on the bus, alongside the registered sink above.
	k.On(toroid.EventNotification, func(_ context.Context, e toroid.Event) error {
		fmt.Printf("[event] %v\n", e.Payload)
		return nil
	})

	out, _, err := k.Run(ctx,
		"List the files here and tell me how many there are, then send a notification "+
			"titled 'done' with the message 'example finished'.")
	if err != nil {
		panic(err)
	}
	fmt.Println("\n" + out)
}
