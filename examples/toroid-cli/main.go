// Pattern: CLI RUNNER — drive the kernel from the command line and emit every
// kernel event as a line of JSON (NDJSON) on stdout.
//
// This is the bridge for hosts written in another language. Wrap this binary in
// a subprocess (e.g. Python's subprocess.Popen) and read stdout line by line:
// each line is one JSON-encoded toroid.Event, so you get the full agentic
// lifecycle — tool calls, results, reasoning, cost, compaction, the final
// answer — without binding to Go. Diagnostics go to stderr so stdout stays a
// clean, machine-parseable event stream.
//
//	export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
//	go run ./examples/toroid-cli 'what files are in this directory?'
//
// Flags (must come before the prompt):
//
//	-model    llm model            (default llmgateway/claude-haiku-4-5)
//	-workdir  working directory    (default current directory)
//	-thinking thinking budget      none | low | high   (default none)
//	-tokens   include per-token Token/Reasoning deltas  (default false)
//	-plain    print only the final assistant response as plain text,
//	          not the NDJSON event stream                (default false)
//
// Example (Python):
//
//	import json, subprocess
//	p = subprocess.Popen(
//	    ["go", "run", "./examples/toroid-cli", "list the files here"],
//	    stdout=subprocess.PIPE, text=True)
//	for line in p.stdout:
//	    ev = json.loads(line)
//	    if ev["kind"] == "AssistantTurn":
//	        ...
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	model := flag.String("model", "llmgateway/claude-haiku-4-5", "llm model name")
	workdir := flag.String("workdir", ".", "working directory")
	thinking := flag.String("thinking", "none", "thinking budget: none | low | high")
	tokens := flag.Bool("tokens", false, "include per-step Reasoning deltas in the stream")
	plain := flag.Bool("plain", false, "print only the final assistant response as plain text, not the NDJSON event stream")
	flag.Parse()

	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: toroid-cli [flags] '<prompt>'")
		flag.PrintDefaults()
		os.Exit(2)
	}

	// The key is resolved per provider prefix by NewKernel: LLM_GATEWAY_KEY for
	// llmgateway/* (with LLM_GATEWAY_BASE_URL), OPENAI_API_KEY for openai/*,
	// ANTHROPIC_API_KEY for anthropic/*.

	ctx := context.Background()
	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                *model,
		WorkDir:              *workdir,
		Thinking:             toroid.Thinking(*thinking),
		IncludeComputerTools: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kernel init:", err)
		os.Exit(1)
	}
	defer k.Close()

	if *plain {
		// -plain: skip the event stream entirely and print just the final
		// answer. This is the same text the AssistantTurn event carries —
		// Run already returns it directly, so there's nothing to subscribe to.
		out, _, err := k.Run(ctx, prompt)
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
		if !*tokens && e.Kind == toroid.EventReasoning {
			return nil // noisy reasoning deltas; opt in with -tokens
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
	if _, _, err := k.Run(ctx, prompt); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}
