// Pattern: Workflow Distillation — capture traces from a powerful model,
// then run the distilled skill on a smaller model.
//
// This example demonstrates:
// 1. Collecting traces from a powerful model (llmgateway/glm-4)
// 2. Running a distilled skill on an edge model (llmgateway/gemma-4b)
// 3. Comparing costs and outputs
//
// Usage:
//
//	export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
//
//	# Phase 1: Collect traces from powerful model
//	go run ./examples/workflow-distillation --collect --model llmgateway/glm-4 \
//	  --prompt "analyze /path/for bottlenecks" > traces.jsonl
//
//	# Phase 2: Run the same workflow on an edge model
//	go run ./examples/workflow-distillation --run --model llmgateway/gemma-4b \
//	  --prompt "analyze /path/for bottlenecks"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

type Config struct {
	mode     string // "collect" or "run"
	model    string
	prompt   string
	workdir  string
	save     bool
	thinking toroid.Thinking
}

func main() {
	cfg := parseFlags()

	ctx := context.Background()
	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                cfg.model,
		APIKey:               os.Getenv("LLM_GATEWAY_KEY"),
		WorkDir:              cfg.workdir,
		Thinking:             cfg.thinking,
		Save:                 cfg.save,
		IncludeComputerTools: true,
	})
	if err != nil {
		log.Fatalf("kernel init: %v", err)
	}
	defer k.Close()

	switch cfg.mode {
	case "collect":
		collectTraces(ctx, k, cfg)
	case "run":
		runSkill(ctx, k, cfg)
	default:
		log.Fatalf("unknown mode: %s", cfg.mode)
	}
}

func parseFlags() Config {
	mode := flag.String("mode", "run", "collect|run")
	model := flag.String("model", "llmgateway/gemma-4b", "model ID")
	prompt := flag.String("prompt", "", "workflow prompt")
	workdir := flag.String("workdir", ".", "working directory")
	save := flag.Bool("save", false, "persist to SQLite")
	thinking := flag.String("thinking", "low", "none|low|high")
	flag.Parse()

	if *prompt == "" {
		log.Fatal("--prompt is required")
	}

	return Config{
		mode:     *mode,
		model:    *model,
		prompt:   *prompt,
		workdir:  *workdir,
		save:     *save,
		thinking: toroid.Thinking(*thinking),
	}
}

// collectTraces runs a workflow on a powerful model and emits all events as NDJSON.
// This is the "record" phase: we're building a trace that shows the model's
// tool-calling strategy, decision points, and outputs.
func collectTraces(ctx context.Context, k *toroid.Kernel, cfg Config) {
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)

	emit := func(_ context.Context, e toroid.Event) error {
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(e)
	}

	// Subscribe to all observable events
	k.OnAll(emit)
	k.On(toroid.EventAssistantTurn, emit)
	k.On(toroid.EventTurnCost, emit)

	// Run the workflow
	fmt.Fprintf(os.Stderr, "=== Collecting trace on model %s ===\n", cfg.model)
	fmt.Fprintf(os.Stderr, "Prompt: %s\n", cfg.prompt)
	fmt.Fprintf(os.Stderr, "\n")

	start := time.Now()
	out, usage, err := k.Run(ctx, cfg.prompt)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Summary to stderr
	fmt.Fprintf(os.Stderr, "\n=== Trace collection complete ===\n")
	fmt.Fprintf(os.Stderr, "Elapsed: %v\n", elapsed)
	fmt.Fprintf(os.Stderr, "Cost: $%.6f\n", k.RunningCostUSD())
	fmt.Fprintf(os.Stderr, "Output preview: %s\n", truncate(out, 200))

	// Print token summary to stderr
	if u, ok := usage.Tokens[k.SessionID()]; ok {
		fmt.Fprintf(os.Stderr, "Tokens: input=%d output=%d\n", u.Input, u.Output)
	}
}

// runSkill runs the same workflow on an edge model.
// In a real system, the prompt would invoke a skill (via the `skill` tool)
// that encodes the patterns learned from tracing.
// For now, we just run the same prompt and compare.
func runSkill(ctx context.Context, k *toroid.Kernel, cfg Config) {
	fmt.Fprintf(os.Stderr, "=== Running on edge model %s ===\n", cfg.model)
	fmt.Fprintf(os.Stderr, "Prompt: %s\n", cfg.prompt)
	fmt.Fprintf(os.Stderr, "\n")

	// For a real distilled skill, you'd prepend:
	//   "You have access to a skill called 'analyze-bottlenecks'. Use it."
	// and rely on the skill's Markdown to guide the model.

	start := time.Now()
	out, usage, err := k.Run(ctx, cfg.prompt)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "\n=== Run complete ===\n")
	fmt.Fprintf(os.Stderr, "Elapsed: %v\n", elapsed)
	fmt.Fprintf(os.Stderr, "Cost: $%.6f\n", k.RunningCostUSD())
	fmt.Fprintf(os.Stderr, "Output preview: %s\n", truncate(out, 200))

	if u, ok := usage.Tokens[k.SessionID()]; ok {
		fmt.Fprintf(os.Stderr, "Tokens: input=%d output=%d\n", u.Input, u.Output)
	}

	// Print the full output
	fmt.Println(out)
}

func truncate(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
