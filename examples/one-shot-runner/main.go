// Pattern: one-shot runner — drive the kernel once with a single prompt and
// exit. This is examples/cli stripped of everything interactive: no REPL, no
// TUI viewport, no /commands, no delegate, no sessions or models subcommands,
// no cbreak/terminal-raw input handling. Just: configure, run, emit, exit.
//
// Output modes:
//
//	default  every kernel event as one JSON object per line (NDJSON) on stdout;
//	         the machine-readable bridge for hosts written in other languages
//	--plain  only the final assistant response as plain text on stdout;
//	         live tool activity goes to stderr as compact one-liners
//
// Configuration:
//
//	env  TOROID_MODEL      provider/model id      (default llmgateway/claude-haiku-4-5)
//	env  TOROID_LLM_TOKEN  API key override       (NewKernel resolves per-provider env otherwise)
//	env  TOROID_MAX_ITER   max tool iterations    (overridden by --max-iter)
//	flag --model           provider/model id
//	flag --thinking        none | low | high      (default low)
//	flag --save/--no-save  persist to the SQLite store (default on)
//	flag --plain           print only the final answer
//	flag --tokens          include Reasoning deltas in the NDJSON stream
//	flag --context-size    total context window size (0 = kernel default)
//	flag --compact-buffer  tokens reserved before auto-compact (0 = kernel default)
//	flag --max-iter        max tool-call iterations (0 = env or kernel default)
//	flag --max-repeat-calls stop after N identical consecutive tool calls
//	flag --smaller-model   cheaper model for compaction/subagents
//	flag --max-spend       max cumulative spend in USD (0 = unlimited)
//	flag --max-tokens      max output tokens per llm-step (0 = provider default)
//
//	export LLM_GATEWAY_BASE_URL=https://openrouter.ai/api/v1
//	export LLM_GATEWAY_KEY=...
//	go run ./examples/one-shot-runner --model stealth/ox-alpha 'what files are here?'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	toroid "github.com/yashbonde/toroid-kernel"
)

type config struct {
	model    string
	workdir  string
	thinking toroid.Thinking
	maxIter  int
	save     bool

	contextSize    int
	compactBuffer  int
	maxRepeatCalls int
	smallerModel   string
	maxSpend       float64
	maxTokens      int

	prompt string
	plain  bool
	tokens bool
}

func loadConfig() (config, string) {
	model := flag.String("model", "", "provider/model id (overrides TOROID_MODEL)")
	save := flag.Bool("save", true, "persist events, costs and metadata to the SQLite store")
	noSave := flag.Bool("no-save", false, "disable persistence (overrides --save)")
	thinking := flag.String("thinking", "low", "thinking budget: none | low | high")
	plain := flag.Bool("plain", false, "print only the final assistant response as plain text")
	tokens := flag.Bool("tokens", false, "include per-step Reasoning deltas in the NDJSON stream")
	contextSize := flag.Int("context-size", 0, "total context window size (0 = kernel default 200000)")
	compactBuffer := flag.Int("compact-buffer", 0, "tokens reserved below context-size before auto-compact fires (0 = kernel default 50000)")
	maxIterFlag := flag.Int("max-iter", 0, "max tool-call iterations per turn (0 = TOROID_MAX_ITER or kernel default 100)")
	maxRepeatCalls := flag.Int("max-repeat-calls", 0, "stop after N consecutive identical tool calls (0 or 1 = guard disabled)")
	smallerModel := flag.String("smaller-model", "", "cheaper model for compaction and subagents (empty = use primary model)")
	maxSpend := flag.Float64("max-spend", 0, "maximum cumulative transcript spend in USD (0 = unlimited)")
	maxTokens := flag.Int("max-tokens", 0, "max output tokens per llm-step (0 = provider default)")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: one-shot-runner [flags] '<prompt>'\n\nDrive the toroid kernel once with a single prompt and exit.\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: a prompt is required")
		flag.Usage()
		os.Exit(2)
	}

	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	if abs, err := filepath.Abs(wd); err == nil {
		wd = abs
	}

	c := config{
		model:          envOr("TOROID_MODEL", "llmgateway/claude-haiku-4-5"),
		workdir:        wd,
		thinking:       toroid.Thinking(*thinking),
		save:           *save && !*noSave,
		prompt:         strings.Join(args, " "),
		plain:          *plain,
		tokens:         *tokens,
		contextSize:    *contextSize,
		compactBuffer:  *compactBuffer,
		maxRepeatCalls: *maxRepeatCalls,
		smallerModel:   *smallerModel,
		maxSpend:       *maxSpend,
		maxTokens:      *maxTokens,
	}
	if *model != "" {
		c.model = *model
	}
	if *maxIterFlag > 0 {
		c.maxIter = *maxIterFlag
	} else if v := os.Getenv("TOROID_MAX_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.maxIter = n
		}
	}
	return c, os.Getenv("TOROID_LLM_TOKEN")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg, apiKey := loadConfig()

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
		fmt.Fprintln(os.Stderr, "kernel init:", err)
		os.Exit(1)
	}
	defer k.Close()

	if cfg.plain {
		runPlain(ctx, k, cfg)
		return
	}
	runNDJSON(ctx, k, cfg)
}

// runPlain prints only the final assistant response on stdout; tool activity
// is summarised to stderr so pipes stay clean.
func runPlain(ctx context.Context, k *toroid.Kernel, cfg config) {
	emit := func(s string) { fmt.Fprint(os.Stderr, s) }
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			emit(fmt.Sprintf("> %s %s\n", p.Name, compact(p.Args, 200)))
		}
		return nil
	})
	k.On(toroid.EventPostToolUseFailure, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			emit(fmt.Sprintf("! %s\n", compact(p.Error, 200)))
		}
		return nil
	})

	out, _, err := k.Run(ctx, cfg.prompt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
	fmt.Println(out)
}

// runNDJSON emits every kernel event as one JSON object per line on stdout.
// Events can fire from subagent goroutines, so writes are serialized to keep
// each object on its own intact line.
func runNDJSON(ctx context.Context, k *toroid.Kernel, cfg config) {
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	k.OnAll(func(_ context.Context, e toroid.Event) error {
		if !cfg.tokens && e.Kind == toroid.EventReasoning {
			return nil // noisy reasoning deltas; opt in with --tokens
		}
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(e)
	})

	if _, _, err := k.Run(ctx, cfg.prompt); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}

// compact collapses whitespace and trims s to max runes.
func compact(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
