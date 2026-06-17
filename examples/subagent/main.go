// Pattern: SUBAGENTS (synchronous delegation).
//
// The built-in `subagent` tool lets the model delegate a self-contained subtask
// to a fresh child kernel that runs to completion and returns its output. The
// child inherits the parent's trace (TraceID) and is recorded as a child span,
// so the whole run is one trace. Subscribe to EventSubagentStart/Stop to observe
// delegations.
//
// You can also delegate directly from Go with kernel.RunSubagent(ctx, task).
//
//	export GEMINI_TOKEN=your_api_key
//	go run ./examples/subagent
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

	k.On(toroid.EventSubagentStart, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok {
			fmt.Printf("[subagent started] %s\n", p.Prompt)
		}
		return nil
	})

	out, _, err := k.Run(ctx,
		"Use a subagent to read the first 10 lines of go.mod, then summarise the module's dependencies.")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
}
