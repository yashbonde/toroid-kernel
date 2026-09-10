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
	"strings"
	"sync"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

// assistantTextFromTurn extracts the assistant's written text from a completed
// turn's structured content ([]llm.Message). The kernel streams a turn's text
// to the writer only for the final, no-tool turn; for tool-calling turns that
// text lives solely in the TurnCompleted event payload, so the CLI renders it
// from there. Returns "" when the payload carries no assistant text.
func assistantTextFromTurn(p *toroid.TurnPayload) string {
	if len(p.Content) == 0 {
		return ""
	}
	var msgs []llm.Message
	if err := json.Unmarshal(p.Content, &msgs); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			sb.WriteString(m.Text())
		}
	}
	return strings.TrimSpace(sb.String())
}

// runOneShot implements `toroid --run '<prompt>'`. It reuses the shared config
// (so --model/--thinking/--save/etc. apply) and the resolved API key.
func runOneShot(cfg config, apiKey string) error {
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
		IncludeSubagentTools: true,

		TotalContextSize:      cfg.contextSize,
		CompactionBufferSize:  cfg.compactBuffer,
		MaxRepeatCalls:        cfg.maxRepeatCalls,
		SmallerModel:          cfg.smallerModel,
		MaxTranscriptSpendUSD: cfg.maxSpend,
		MaxTokens:             cfg.maxTokens,
	})
	if err != nil {
		return fmt.Errorf("kernel init: %w", err)
	}
	defer k.Close()

	if cfg.plain {
		// --plain skips the event stream and renders the complete final answer
		// through the same spacing/wrapping path as the interactive transcript.
		// Unlike the viewport it is not clipped, so it also serves as a faithful
		// headless rendering probe.
		//
		// stdout stays purely the final answer; live tool activity goes to
		// stderr through the same box renderer the REPL uses.
		emitPlain := func(s string) { fmt.Fprint(os.Stderr, s) }
		// The kernel writes a turn's text to the stream only for the final,
		// no-tool turn. Render the intermediate assistant text from the
		// TurnCompleted event so the reader sees the model's running narration
		// alongside the tool activity, not just the final answer.
		k.On(toroid.EventTurnCompleted, func(_ context.Context, e toroid.Event) error {
			if p, ok := e.Payload.(*toroid.TurnPayload); ok && p.ToolCalls > 0 {
				if text := assistantTextFromTurn(p); text != "" {
					emitPlain(aGray + text + aReset + "\n")
				}
			}
			return nil
		})
		k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
			if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
				emitPlain(renderToolCall(p.Name, p.Args, cfg.workdir, termWidth()))
			}
			return nil
		})
		k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
			if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
				emitPlain(toolResultLine(p.Result))
			}
			return nil
		})
		k.On(toroid.EventPostToolUseFailure, func(_ context.Context, e toroid.Event) error {
			if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
				emitPlain(toolErrorLine(p.Error))
			}
			return nil
		})

		out, _, err := k.Run(ctx, withAgentsReminder(cfg.run, cfg.workdir))
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}
		fmt.Println(renderAssistantText(out, max(1, termWidth())))
		return nil
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

	// OnAll now covers the full lifecycle, including the model's actual output:
	// TurnCompleted carries the turn's structured content and cost directly
	// (what separate AssistantTurn/TurnCost subscriptions used to be needed for).
	k.OnAll(emit)

	// Drive the loop. We discard the writer copy of the final text because the
	// EventStop / EventTurnCompleted events already carry it on the stream.
	if _, _, err := k.Run(ctx, withAgentsReminder(cfg.run, cfg.workdir)); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}
