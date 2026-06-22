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
//	export ANTHROPIC_API_KEY=your_api_key
//	go run ./examples/toroid-cli 'what files are in this directory?'
//
// Flags (must come before the prompt):
//
//	-model    llm model            (default anthropic/claude-haiku-4-5)
//	-workdir  working directory    (default current directory)
//	-thinking thinking budget      none | low | high   (default none)
//	-tokens   include per-token Token/Reasoning deltas  (default false)
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
	model := flag.String("model", "anthropic/claude-haiku-4-5", "llm model name")
	workdir := flag.String("workdir", ".", "working directory")
	thinking := flag.String("thinking", "none", "thinking budget: none | low | high")
	tokens := flag.Bool("tokens", false, "include per-step Reasoning deltas in the stream")
	flag.Parse()

	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: toroid-cli [flags] '<prompt>'")
		flag.PrintDefaults()
		os.Exit(2)
	}

	// Pick the API-key env var from the model's provider prefix so the same
	// binary works across providers. For llmgateway you must also set
	// LLM_GATEWAY_BASE_URL (the OpenAI-compatible base, including "/v1").
	keyEnv := apiKeyEnvFor(*model)
	apiKey := os.Getenv(keyEnv)
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "set %s to run\n", keyEnv)
		os.Exit(1)
	}

	ctx := context.Background()
	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:    *model,
		APIKey:   apiKey,
		WorkDir:  *workdir,
		Thinking: toroid.Thinking(*thinking),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kernel init:", err)
		os.Exit(1)
	}
	defer k.Close()

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

// apiKeyEnvFor maps a "provider/model" id to the env var holding that provider's
// API key, mirroring toroid.NewProviderFromLLMId.
func apiKeyEnvFor(model string) string {
	provider, _, _ := strings.Cut(model, "/")
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "llmgateway":
		return "LLM_GATEWAY_KEY"
	default: // google or unprefixed
		return "GEMINI_TOKEN"
	}
}
