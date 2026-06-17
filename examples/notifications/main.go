// Pattern: NOTIFICATIONS (pluggable sinks).
//
// The `notify` tool fires an EventNotification on the bus AND runs any sinks
// registered with tools.RegisterNotifySink, plus a best-effort desktop
// notification. Register your own sink to route notifications to a webhook,
// Slack, or a peer kernel — the tool itself stays platform-agnostic.
//
//	export GEMINI_TOKEN=your_api_key
//	go run ./examples/notifications
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/tools"
)

func main() {
	apiKey := os.Getenv("GEMINI_TOKEN")
	if apiKey == "" {
		fmt.Println("set GEMINI_TOKEN to run this example")
		return
	}
	ctx := context.Background()

	// Register a custom sink BEFORE constructing the kernel. In a real host this
	// might POST to a webhook or forward to a peer kernel.
	tools.RegisterNotifySink(func(_ context.Context, title, message string) error {
		fmt.Printf("[sink] %s: %s\n", title, message)
		return nil
	})

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:   "google/gemini-3-flash-preview",
		APIKey:  apiKey,
		WorkDir: ".",
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	// You can also observe notifications directly on the event bus.
	k.On(toroid.EventNotification, func(_ context.Context, e toroid.Event) error {
		fmt.Printf("[event] %v\n", e.Payload)
		return nil
	})

	out, _, err := k.Run(ctx, "Send a notification titled 'done' with the message 'example finished'.")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
}
