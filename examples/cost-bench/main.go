// cost-bench runs a multi-step tool-using turn and prints spend metrics.
//
//	export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=... TOROID_MODEL=llmgateway/kimi-k2p6
//	go run ./examples/cost-bench
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	model := os.Getenv("TOROID_MODEL")
	if model == "" {
		model = "llmgateway/kimi-k2p6"
	}
	key := os.Getenv("LLM_GATEWAY_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "set LLM_GATEWAY_KEY (or OPENAI_API_KEY)")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	label := os.Getenv("COST_BENCH_LABEL")
	if label == "" {
		label = "run"
	}

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                model,
		APIKey:               key,
		WorkDir:              mustRepoRoot(),
		Save:                 false,
		IncludeComputerTools: true,
		LoadSkills:           boolPtr(false),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewKernel: %v\n", err)
		os.Exit(1)
	}
	defer k.Close()

	var steps, unpricedSteps int
	var inputTok, outputTok, cacheRead, cacheWrite int64
	k.On(toroid.EventTurnCost, func(_ context.Context, e toroid.Event) error {
		steps++
		if p, ok := e.Payload.(*toroid.TurnCostPayload); ok {
			inputTok += p.TurnUsage.Input
			outputTok += p.TurnUsage.Output
			cacheRead += p.TurnUsage.CacheRead
			cacheWrite += p.TurnUsage.CacheWrite
			// A step whose model is absent from pricing.json reports $0 with
			// PricingOK=false — count these so a low total isn't mistaken for cheap.
			if !p.TurnUsage.PricingOK {
				unpricedSteps++
			}
		}
		return nil
	})

	prompt := strings.TrimSpace(`
You are measuring harness cost. Complete this checklist with tools (no subagents):

1. Use ls on the tools/ directory.
2. Use grep to search the repo for the pattern "EventTurnCost" (path .).
3. Use grep to search for the pattern "toroid-kernel/llm" under tools/ and kernel.go if needed.
4. Use glob for "**/*.go" under tools/ (or find Go files there).
5. Read the first ~80 lines of kernel.go.
6. Reply with a short bullet list: how many tool steps you took, one sentence on what EventTurnCost is, and the names of 3 tools you used.

Be concise. Prefer tools over speculation.
`)

	start := time.Now()
	out, usage, err := k.Run(ctx, prompt)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Run error: %v\n", err)
	}

	// Sum usage map if present
	var sessionCost float64
	for _, u := range usage.Tokens {
		sessionCost += u.Cost
	}

	fmt.Printf("=== cost-bench %s ===\n", label)
	fmt.Printf("model=%s\n", model)
	fmt.Printf("elapsed=%s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("llm_steps=%d\n", steps)
	fmt.Printf("tokens_input=%d output=%d cache_read=%d cache_write=%d\n",
		inputTok, outputTok, cacheRead, cacheWrite)
	fmt.Printf("cost_running_usd=%.6f\n", k.RunningCostUSD())
	fmt.Printf("cost_usage_map_usd=%.6f\n", sessionCost)
	if unpricedSteps > 0 {
		fmt.Printf("pricing=UNAVAILABLE for %d/%d step(s): model %q missing from pricing.json; $ above is an underestimate, not free\n",
			unpricedSteps, steps, model)
	} else {
		fmt.Printf("pricing=ok (all %d step(s) resolved to a pricing row)\n", steps)
	}
	ctxUsed, ctxTotal := k.ContextUsage()
	fmt.Printf("context_used=%d context_total=%d\n", ctxUsed, ctxTotal)
	fmt.Printf("output_chars=%d\n", len(out))
	if err != nil {
		fmt.Printf("error=%v\n", err)
		os.Exit(1)
	}
}

func boolPtr(b bool) *bool { return &b }

func mustRepoRoot() string {
	// Prefer repo root when run via `go run ./examples/cost-bench` from module root.
	if _, err := os.Stat("kernel.go"); err == nil {
		wd, _ := os.Getwd()
		return wd
	}
	wd, _ := os.Getwd()
	return wd
}
