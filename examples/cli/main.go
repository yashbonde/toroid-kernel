// Pattern: REPL — an interactive, pretty-printing chat loop for talking to any
// agent the kernel can drive.
//
// This is the human-facing counterpart to --run (`toroid --run '<prompt>'`,
// which emits machine NDJSON — see run.go).
// You type, the agent answers, and the answer is rendered: Markdown is formatted
// (headings, **bold**, `code`, fenced blocks, lists), tool calls are shown as
// compact one-liners with their args/results trimmed so a chatty tool never eats
// your whole screen, and per-turn cost is tracked in the prompt.
//
// Configuration is split between environment variables (the "what to talk to"
// knobs) and command-line flags (the per-run toggles):
//
//	env  TOROID_MODEL      provider/model id      (default llmgateway/claude-haiku-4-5)
//	env  TOROID_LLM_TOKEN  API key for the provider (required)
//	env  TOROID_MAX_ITER   max tool iterations    (default kernel default, 100)
//	env  TOROID_TRIM       max chars per tool arg/result line (default 120)
//	flag --model           override TOROID_MODEL  (default "")
//	flag --save            persist events/costs to the SQLite store (default off)
//	flag --thinking        none | low | high      (default low)
//	flag --no-colour       disable all ANSI styling (default off)
//	flag --context-size    total context window size (0 = use kernel default)
//	flag --compact-buffer  tokens reserved before auto-compact triggers (0 = use kernel default)
//	flag --max-iter        max tool-call iterations per turn (0 = use env or kernel default)
//	flag --max-repeat-calls stop after N consecutive identical tool calls (0 or 1 disables the guard)
//	flag --smaller-model   cheaper model for compaction/subagents (empty = use primary)
//	flag --max-spend       max cumulative spend in USD (0 = unlimited)
//
//	export TOROID_LLM_TOKEN=your_api_key
//	go run ./examples/cli --model openai/gpt-4o --thinking high --save
//
// In-REPL commands: /help /cost /model /reset /clear /exit  (or Ctrl-D to quit).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tsize "github.com/kopoli/go-terminal-size"
	toroid "github.com/yashbonde/toroid-kernel"
)

var (
	reasoningActive       bool
	reasoningNeedsNewline bool
)

type config struct {
	model    string
	workdir  string
	thinking toroid.Thinking
	maxIter  int
	save     bool
	trim     int

	// Context/compaction knobs from CLI flags; zero/empty means "use kernel default".
	contextSize    int
	compactBuffer  int
	maxRepeatCalls int
	smallerModel   string
	maxSpend       float64
	maxTokens      int

	// One-shot mode (--run). When run is non-empty the binary drives the kernel
	// once, emitting NDJSON events (or --plain for just the final answer),
	// instead of starting the interactive REPL. --model/--thinking/--save all
	// apply, since --run shares this flag set.
	run    string
	plain  bool
	tokens bool
}

// loadConfig reads env vars (model/token/iter/trim) and parses the flags
// (--model, --save, --thinking, --no-colour, --run, --plain, --tokens). It
// returns the config plus the resolved API key. Flags win for the per-run
// toggles; env wins for the targeting knobs.
func loadConfig() (config, string) {
	model := flag.String("model", "", "override TOROID_MODEL (provider/model)")
	save := flag.Bool("save", false, "persist events, costs and metadata to the SQLite store")
	thinking := flag.String("thinking", "low", "thinking budget: none | low | high")
	noColour := flag.Bool("no-colour", false, "disable ANSI colour/styling")
	run := flag.String("run", "", "one-shot: run this prompt, emit NDJSON events, and exit (non-interactive)")
	plain := flag.Bool("plain", false, "with --run: print only the final assistant response as plain text, not the NDJSON event stream")
	tokens := flag.Bool("tokens", false, "with --run: include per-step Reasoning deltas in the NDJSON stream")

	// Context/compaction knobs
	contextSize := flag.Int("context-size", 0, "total context window size (0 = kernel default 200000)")
	compactBuffer := flag.Int("compact-buffer", 0, "tokens reserved below context-size before auto-compact fires (0 = kernel default 50000)")
	maxIterFlag := flag.Int("max-iter", 0, "max tool-call iterations per turn (0 = use TOROID_MAX_ITER or kernel default 100)")
	maxRepeatCalls := flag.Int("max-repeat-calls", 0, "stop after N consecutive identical tool calls (0 or 1 = guard disabled)")
	smallerModel := flag.String("smaller-model", "", "cheaper model for compaction and subagents (empty = use primary model)")
	maxSpend := flag.Float64("max-spend", 0, "maximum cumulative transcript spend in USD (0 = unlimited)")
	maxTokens := flag.Int("max-tokens", 0, "max output tokens per llm-step (0 = provider default)")
	flag.Parse()

	if *noColour {
		disableColor()
	}

	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	absWd, err := filepath.Abs(wd)
	if err != nil {
		absWd = wd
	}

	// Flag --model overrides env TOROID_MODEL
	modelStr := envOr("TOROID_MODEL", "llmgateway/claude-haiku-4-5")
	if *model != "" {
		modelStr = *model
	}

	c := config{
		model:    modelStr,
		workdir:  absWd,
		thinking: toroid.Thinking(*thinking),
		save:     *save,
		trim:     120,
		run:      strings.TrimSpace(*run),
		plain:    *plain,
		tokens:   *tokens,

		contextSize:    *contextSize,
		compactBuffer:  *compactBuffer,
		maxRepeatCalls: *maxRepeatCalls,
		smallerModel:   *smallerModel,
		maxSpend:       *maxSpend,
		maxTokens:      *maxTokens,
	}
	// --max-iter flag wins; TOROID_MAX_ITER env is the fallback; 0 means kernel default.
	if *maxIterFlag > 0 {
		c.maxIter = *maxIterFlag
	} else {
		if v := os.Getenv("TOROID_MAX_ITER"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				c.maxIter = n
			}
		}
	}
	if v := os.Getenv("TOROID_TRIM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 10 {
			c.trim = n
		}
	}
	return c, os.Getenv("TOROID_LLM_TOKEN") // optional override; NewKernel resolves per-provider env otherwise
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func termWidth() int {
	if s, err := tsize.GetSize(); err == nil && s.Width > 0 {
		return s.Width
	}
	return 80
}

func main() {
	// apiKey may be empty: NewKernel resolves the key per provider prefix
	// (LLM_GATEWAY_KEY / OPENAI_API_KEY / ANTHROPIC_API_KEY); TOROID_LLM_TOKEN
	// is an explicit override.
	cfg, apiKey := loadConfig()

	// --run: one-shot, non-interactive. Drive the kernel once and emit every
	// event as NDJSON (or --plain for just the final answer) — the bridge for
	// hosts in other languages. All targeting flags (--model, --thinking,
	// --save, …) apply since --run shares the same flag set.
	if cfg.run != "" {
		if err := runOneShot(cfg, apiKey); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	ctx := context.Background()
	k, err := newKernel(ctx, cfg, apiKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kernel init:", err)
		os.Exit(1)
	}

	banner(cfg)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Print(aGreen + aBold + "\nyou ▸ " + aReset)
		if !in.Scan() {
			fmt.Println()
			break // EOF (Ctrl-D)
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}

		// Meta commands start with '/'.
		if strings.HasPrefix(line, "/") {
			quit, reset := handleCommand(line, k, cfg)
			if quit {
				break
			}
			if reset {
				_ = k.Close()
				if k, err = newKernel(ctx, cfg, apiKey); err != nil {
					fmt.Fprintln(os.Stderr, "reset failed:", err)
					return
				}
			}
			continue
		}

		ask(ctx, k, cfg, line)
	}
	_ = k.Close()
}

// newKernel wires a kernel plus all the pretty-printing event hooks. Recreated on
// /reset to start a fresh session.
func newKernel(ctx context.Context, cfg config, apiKey string) (*toroid.Kernel, error) {
	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                cfg.model,
		APIKey:               apiKey,
		WorkDir:              cfg.workdir,
		Thinking:             cfg.thinking,
		MaxIter:              cfg.maxIter,
		Save:                 cfg.save,
		IncludeComputerTools: true,

		TotalContextSize:      cfg.contextSize,
		CompactionBufferSize:  cfg.compactBuffer,
		MaxRepeatCalls:        cfg.maxRepeatCalls,
		SmallerModel:          cfg.smallerModel,
		MaxTranscriptSpendUSD: cfg.maxSpend,
		MaxTokens:             cfg.maxTokens,
	})
	if err != nil {
		return nil, err
	}

	// Tool call begins: compact one-liner, args trimmed to the configured width.
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			if reasoningActive && reasoningNeedsNewline {
				fmt.Println()
				reasoningNeedsNewline = false
			}
			fmt.Printf("  %s⚙ %s%s%s %s\n", aBlue, aBold, p.Name, aReset, dimArgs(p.Args, cfg.trim))
		}
		return nil
	})
	// Tool call result: a short preview, or the error in red.
	k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			if reasoningActive && reasoningNeedsNewline {
				fmt.Println()
				reasoningNeedsNewline = false
			}
			fmt.Printf("  %s→ %s%s\n", aDim, trimOneLine(p.Result, cfg.trim), aReset)
		}
		return nil
	})
	k.On(toroid.EventPostToolUseFailure, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			if reasoningActive && reasoningNeedsNewline {
				fmt.Println()
				reasoningNeedsNewline = false
			}
			fmt.Printf("  %s→ error: %s%s\n", aRed, trimOneLine(p.Error, cfg.trim), aReset)
		}
		return nil
	})
	k.On(toroid.EventReasoning, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ReasoningPayload); ok {
			if !reasoningActive {
				reasoningActive = true
				fmt.Println(aGray + strings.Repeat("—", termWidth()) + aReset)
			}
			fmt.Print(aGray + aItalic + p.Text + aReset)
			reasoningNeedsNewline = !strings.HasSuffix(p.Text, "\n")
		}
		return nil
	})
	return k, nil
}

// ask drives one turn: run the prompt (blocking), then render the Markdown answer
// and the running cost. Tool activity streams live via the hooks above.
func ask(ctx context.Context, k *toroid.Kernel, cfg config, prompt string) {
	width := termWidth()

	ctx, cancel := context.WithCancel(ctx)

	// Put terminal in cbreak mode so ESC is readable immediately.
	fd := int(os.Stdin.Fd())
	oldState, err := enableCBreak(fd)
	if err == nil {
		done := make(chan struct{})
		go watchEsc(ctx, fd, cancel, done)
		// cancel() must happen, and watchEsc must have observed it and
		// stopped touching fd, before we hand stdin back to the REPL's
		// line-buffered scanner — otherwise both read the same fd at once.
		defer func() {
			cancel()
			<-done
			restoreCBreak(fd, oldState)
		}()
	} else {
		defer cancel()
	}

	start := time.Now()
	out, usage, err := k.Run(ctx, prompt)
	elapsed := time.Since(start)
	if reasoningActive {
		if reasoningNeedsNewline {
			fmt.Println()
		}
		fmt.Println(aGray + strings.Repeat("—", width) + aReset)
		reasoningActive = false
		reasoningNeedsNewline = false
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "%srun error: %v%s\n", aRed, err, aReset)
		return
	}
	fmt.Printf("\n%s%s ◂%s\n", aMagenta+aBold, shortModel(cfg.model), aReset)
	fmt.Print(renderMarkdown(out, width))

	// Turn footer: running cost in USD (gateway-reported) plus token usage and throughput.
	// EventStop's UsagePayload carries the session's per-turn usage by session ID.
	fmt.Printf("%s  $%.6f%s", aGray, k.RunningCostUSD(), aReset)

	// Print token usage and cache statistics if available.
	if u, ok := usage.Tokens[k.SessionID()]; ok && (u.Input > 0 || u.CacheRead > 0 || u.CacheWrite > 0 || u.Output > 0) {
		var parts []string
		if u.Input > 0 {
			parts = append(parts, fmt.Sprintf("%d←", u.Input))
		}
		if u.CacheRead > 0 {
			parts = append(parts, fmt.Sprintf("%d📥", u.CacheRead))
		}
		if u.CacheWrite > 0 {
			parts = append(parts, fmt.Sprintf("%d📤", u.CacheWrite))
		}
		total := u.Input + u.CacheRead + u.CacheWrite + u.Output
		parts = append(parts, fmt.Sprintf("%dΣ", total))
		if len(parts) > 0 {
			fmt.Printf("%s  ·  %s%s", aGray, strings.Join(parts, " "), aReset)
		}
	}

	if outTokens := usage.Tokens[k.SessionID()].Output; outTokens > 0 && elapsed.Seconds() > 0 {
		fmt.Printf("%s  ·  %d→ in %.1fs (%.1f tok/s)%s", aGray, outTokens, elapsed.Seconds(), float64(outTokens)/elapsed.Seconds(), aReset)
	}
	fmt.Println()
}

// dimArgs renders tool args as compact dim text, trimmed. Tool args are JSON;
// we don't pretty-print, just keep them to one short line.
func dimArgs(args string, max int) string {
	return aDim + trimOneLine(args, max) + aReset
}

func handleCommand(line string, k *toroid.Kernel, cfg config) (quit, reset bool) {
	switch strings.ToLower(strings.Fields(line)[0]) {
	case "/exit", "/quit", "/q":
		return true, false
	case "/reset", "/new":
		fmt.Println(aGray + "— started a fresh session —" + aReset)
		return false, true
	case "/clear":
		fmt.Print("\x1b[2J\x1b[H")
	case "/cost":
		fmt.Printf("%scost so far: $%.6f%s\n", aYellow, k.RunningCostUSD(), aReset)
	case "/model":
		fmt.Printf("%smodel: %s | workdir: %s | thinking: %s%s\n",
			aYellow, cfg.model, displayWorkdir(cfg.workdir), cfg.thinking, aReset)
		printModelExtras(cfg)
	case "/help", "/?":
		printHelp()
	default:
		fmt.Printf("%sunknown command %q — try /help%s\n", aRed, line, aReset)
	}
	return false, false
}

func banner(cfg config) {
	fmt.Printf("%s┌─────────────────────────────────────────────%s\n", aCyan, aReset)
	fmt.Printf("%s│ toroid repl%s  %s%s%s\n", aCyan+aBold, aReset, aGray, shortModel(cfg.model), aReset)
	fmt.Printf("%s│%s thinking=%s save=%v workdir=%s\n", aCyan, aReset, cfg.thinking, cfg.save, displayWorkdir(cfg.workdir))
	fmt.Printf("%s└ %stype a message, or /help · /exit%s\n", aCyan, aGray, aReset)
}

func printHelp() {
	fmt.Print(aGray + `commands:
  /help        show this help
  /cost        running cost in USD for this session
  /model       show active model & config
  /reset       start a fresh session (clears history)
  /clear       clear the screen
  /exit        quit (or Ctrl-D)

config via env:   TOROID_MODEL, TOROID_LLM_TOKEN, TOROID_MAX_ITER, TOROID_TRIM
config via flags: --model, --save, --thinking (none|low|high), --no-colour
                  --context-size, --compact-buffer, --max-iter, --max-repeat-calls,
                  --smaller-model, --max-spend
` + aReset)
}

// displayWorkdir returns a path relative to the home directory when possible.
func displayWorkdir(wd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return wd
	}
	if strings.HasPrefix(wd, home) {
		return "~" + strings.TrimPrefix(wd, home)
	}
	return wd
}

// shortModel drops the provider prefix for a tidier label.
func shortModel(m string) string {
	if _, rest, ok := strings.Cut(m, "/"); ok {
		return rest
	}
	return m
}

// printModelExtras prints the context/compaction knobs when they differ from defaults.
func printModelExtras(cfg config) {
	var parts []string
	if cfg.contextSize > 0 {
		parts = append(parts, fmt.Sprintf("context=%d", cfg.contextSize))
	}
	if cfg.compactBuffer > 0 {
		parts = append(parts, fmt.Sprintf("buffer=%d", cfg.compactBuffer))
	}
	if cfg.maxIter > 0 {
		parts = append(parts, fmt.Sprintf("max_iter=%d", cfg.maxIter))
	}
	if cfg.maxRepeatCalls > 0 {
		parts = append(parts, fmt.Sprintf("max_repeat=%d", cfg.maxRepeatCalls))
	}
	if cfg.smallerModel != "" {
		parts = append(parts, fmt.Sprintf("smaller=%s", shortModel(cfg.smallerModel)))
	}
	if cfg.maxSpend > 0 {
		parts = append(parts, fmt.Sprintf("max_spend=$%.6f", cfg.maxSpend))
	}
	if len(parts) > 0 {
		fmt.Printf("  %s%s%s\n", aGray, strings.Join(parts, " | "), aReset)
	}
}
