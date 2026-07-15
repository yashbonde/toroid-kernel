// One-shot mode: `toroid --run '<prompt>'` — the machine-facing counterpart to
// the interactive REPL. It drives the kernel once and emits every kernel event
// as a line of JSON (NDJSON) on stdout.
//
// This is the bridge for hosts written in another language. Wrap the binary in
// a subprocess (e.g. Python's subprocess.Popen) and read stdout line by line:
// each line is one JSON-encoded toroid.Event, so you get the full agentic
// lifecycle — tool calls, results, reasoning, cost, compaction, the final
// answer — without binding to Go. Diagnostics go to stderr so stdout stays a
// clean, machine-parseable event stream.
//
// --run shares the REPL's flag set, so all the targeting flags apply:
//
//	export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
//	go run ./examples/cli --run 'what files are in this directory?'
//	go run ./examples/cli --model openai/gpt-4o --thinking high --run 'summarise'
//	go run ./examples/cli --plain --run 'just the final answer, as text'
//
// The --run-specific toggles:
//
//	--tokens  include per-step Reasoning deltas in the stream  (default false)
//	--plain   print only the final assistant response as plain text,
//	          not the NDJSON event stream                       (default false)
//
// Example (Python):
//
//	import json, subprocess
//	p = subprocess.Popen(
//	    ["go", "run", "./examples/cli", "--run", "list the files here"],
//	    stdout=subprocess.PIPE, text=True)
//	for line in p.stdout:
//	    ev = json.loads(line)
//	    if ev["kind"] == "AssistantTurn":
//	        ...
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	toroid "github.com/yashbonde/toroid-kernel"
)

// runOneShot implements `toroid --run '<prompt>'`. It reuses the shared config
// (so --model/--thinking/--save/etc. apply) and the resolved API key.
func runOneShot(cfg config, apiKey string) {
	// The key is resolved per provider prefix by NewKernel: LLM_GATEWAY_KEY for
	// llmgateway/* (with LLM_GATEWAY_BASE_URL), OPENAI_API_KEY for openai/*,
	// ANTHROPIC_API_KEY for anthropic/*. apiKey (TOROID_LLM_TOKEN) overrides.

	ctx := context.Background()
	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                cfg.model,
		APIKey:               apiKey,
		WorkDir:              cfg.workdir,
		Thinking:             cfg.thinking,
		MaxIter:              cfg.maxIter,
		Save:                 cfg.save,
		IncludeComputerTools: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kernel init:", err)
		os.Exit(1)
	}
	defer k.Close()

	if cfg.plain {
		// --plain: skip the event stream entirely and print just the final
		// answer. This is the same text the AssistantTurn event carries —
		// Run already returns it directly, so there's nothing to subscribe to.
		out, _, err := k.Run(ctx, cfg.run)
		if err != nil {
			fmt.Fprintln(os.Stderr, "run:", err)
			os.Exit(1)
		}
		fmt.Println(out)
		return
	}

	// stdout is a single shared stream; events can fire from subagent goroutines,
	// so serialize writes to keep each JSON object on its own intact line.
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)

	emit := func(_ context.Context, e toroid.Event) error {
		if !cfg.tokens && e.Kind == toroid.EventReasoning {
			return nil // noisy reasoning deltas; opt in with --tokens
		}
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(e) // Encode appends a newline -> NDJSON
	}

	// OnAll covers most of the lifecycle, but it deliberately omits the events
	// that carry the model's actual output — AssistantTurn (the full final text
	// and structured content) and TurnCost — so subscribe to those explicitly.
	// Without this you'd see tool calls and a Stop event but never the answer.
	k.OnAll(emit)
	k.On(toroid.EventAssistantTurn, emit)
	k.On(toroid.EventTurnCost, emit)

	// Drive the loop. We discard the writer copy of the final text because the
	// EventStop / EventAssistantTurn events already carry it on the stream.
	if _, _, err := k.Run(ctx, cfg.run); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}
