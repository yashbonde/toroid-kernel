// Pattern: STREAMING run.
//
// kernel.Stream writes the assistant's final text to an io.Writer as it is
// produced. To observe individual token deltas (e.g. to render a live UI), also
// subscribe to EventToken. Use this for interactive/TUI hosts.
//
//	export GEMINI_TOKEN=your_api_key
//	go run ./examples/streaming
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

	// Token deltas arrive here as they stream; this is the hook a UI would use.
	k.On(toroid.EventToken, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TokenPayload); ok {
			fmt.Print(p.Text)
		}
		return nil
	})

	// Stream also writes the final text to the provided writer.
	if err := k.Stream(ctx, "Count from 1 to 5, one number per line.", os.Stdout); err != nil {
		panic(err)
	}
	fmt.Println()
}
