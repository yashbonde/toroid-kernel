package toroid

import (
	"context"
	"io"
	"sync"
	"testing"
)

// A user prompt arriving while a background delegation wakes the kernel must not
// race the wake-up on History. Both used to append to the same slice from
// different goroutines, which loses turns or corrupts the slice; the claim in
// Stream now routes the loser through the message queue instead.
//
// Run with -race: without the claim this reports a write/write data race on
// Kernel.History.
func TestConcurrentStreamAndBackgroundWakeDoNotRaceOnHistory(t *testing.T) {
	loadSkills := false
	k, err := NewKernel(context.Background(), Config{
		Model:                "llmgateway/test-model",
		WorkDir:              t.TempDir(),
		LoadSkills:           &loadSkills,
		IncludeComputerTools: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	k.Step = &FauxStep{Replies: []FauxReply{{Text: "ok", StopReason: "stop"}}}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		// A caller streaming a prompt.
		go func() {
			defer wg.Done()
			_ = k.Stream(context.Background(), "user prompt", io.Discard)
		}()
		// A background task finishing and waking the kernel.
		go func(n int) {
			defer wg.Done()
			k.Enqueue("[background task done]")
			_ = k.tryResume(context.Background())
		}(i)
	}
	wg.Wait()

	// Every message must still be a well-formed entry — a lost append would
	// leave a hole and a torn slice would panic before reaching here.
	for i, m := range k.History {
		if m.Role == "" {
			t.Fatalf("history[%d] has no role: the slice was corrupted", i)
		}
	}
}

// A caller that does not win the loop claim must leave the flag alone, and a
// prompt it queued must still be delivered.
func TestStreamQueuesWhenLoopIsAlreadyRunning(t *testing.T) {
	k := &Kernel{Cfg: Config{SessionID: "s", TraceID: "s"}, Hooks: &HookRegistry{}, Sessions: map[string]Usage{}}
	k.running.Store(true) // a loop already owns the kernel

	if err := k.Stream(context.Background(), "queued prompt", io.Discard); err != nil {
		t.Fatalf("Stream returned %v, want nil", err)
	}
	if len(k.History) != 0 {
		t.Fatalf("History was mutated by a caller that did not own the loop: %v", k.History)
	}
	if got := k.drainQueue(); len(got) != 1 || got[0] != "queued prompt" {
		t.Fatalf("queue = %v, want the prompt to have been queued", got)
	}
	if !k.running.Load() {
		t.Fatal("the losing caller cleared the running flag out from under the live loop")
	}
}
