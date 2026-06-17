// Pattern: BACKGROUND AGENTS (async delegation + wake-on-completion).
//
// SpawnBackground runs a subtask asynchronously and returns a task id
// immediately. When it finishes, its result is queued and the kernel is woken to
// process it — even if Run/Stream had already returned (the kernel was idle).
// The model can also trigger this itself via the `subagent_async` tool.
//
// Observe progress with EventTaskCompleted (the background task finished) and
// EventMasterIdle (a turn ended with nothing left queued).
//
//	export GEMINI_TOKEN=your_api_key
//	go run ./examples/background
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

	// Fire off a background task; this returns immediately.
	id := k.SpawnBackground("Count the number of .go files in the working directory.")
	fmt.Printf("spawned background task %s; doing other work…\n", id)

	// When the task completes it wakes the (idle) kernel; wait for that turn to end.
	<-done
	fmt.Println("background work absorbed by the main kernel")
}
