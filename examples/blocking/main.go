// Pattern: BLOCKING run.
//
// kernel.Run executes the full agent loop and returns the complete final text in
// one call. Use this when you just want the answer and don't need to stream
// tokens. Run also returns a UsagePayload (per-session token totals).
//
//	export GEMINI_TOKEN=your_api_key
//	go run ./examples/blocking
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

	out, usage, err := k.Run(ctx, "In one sentence, what is in this directory?")
	if err != nil {
		panic(err)
	}

	fmt.Println(out)
	fmt.Printf("\nsessions billed: %d | cost: $%.6f\n", len(usage.Tokens), k.RunningCostUSD())
}
