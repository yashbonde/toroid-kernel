// Pattern: REPL — an interactive, pretty-printing chat loop for talking to any
// agent the kernel can drive.
//
// This is the human-facing counterpart to toroid-cli (which emits machine NDJSON).
// You type, the agent answers, and the answer is rendered: Markdown is formatted
// (headings, **bold**, `code`, fenced blocks, lists), tool calls are shown as
// compact one-liners with their args/results trimmed so a chatty tool never eats
// your whole screen, and per-turn cost is tracked in the prompt.
//
// Configuration is split between environment variables (the "what to talk to"
// knobs) and command-line flags (the per-run toggles):
//
//	env  TOROID_MODEL      provider/model id      (default anthropic/claude-haiku-4-5)
//	env  TOROID_LLM_TOKEN  API key for the provider (required)
//	env  TOROID_MAX_ITER   max tool iterations    (default kernel default, 50)
//	env  TOROID_TRIM       max chars per tool arg/result line (default 120)
//	flag --save            persist events/costs to the SQLite store (default off)
//	flag --thinking        none | low | high      (default low)
//	flag --no-colour       disable all ANSI styling (default off)
//
//	export TOROID_LLM_TOKEN=your_api_key
//	go run ./examples/toroid-repl --thinking high --save
//
// In-REPL commands: /help /cost /model /reset /clear /exit  (or Ctrl-D to quit).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	tsize "github.com/kopoli/go-terminal-size"
	toroid "github.com/yashbonde/toroid-kernel"
)

type config struct {
	model    string
	workdir  string
	thinking toroid.Thinking
	maxIter  int
	save     bool
	trim     int
}

// loadConfig reads env vars (model/token/iter/trim) and parses the flags
// (--save, --thinking, --no-colour). It returns the config plus the resolved
// API key. Flags win for the per-run toggles; env wins for the targeting knobs.
func loadConfig() (config, string) {
	save := flag.Bool("save", false, "persist events, costs and metadata to the SQLite store")
	thinking := flag.String("thinking", "low", "thinking budget: none | low | high")
	noColour := flag.Bool("no-colour", false, "disable ANSI colour/styling")
	flag.Parse()

	if *noColour {
		disableColor()
	}

	c := config{
		model:    envOr("TOROID_MODEL", "anthropic/claude-haiku-4-5"),
		workdir:  ".",
		thinking: toroid.Thinking(*thinking),
		save:     *save,
		trim:     120,
	}
	if v := os.Getenv("TOROID_MAX_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.maxIter = n
		}
	}
	if v := os.Getenv("TOROID_TRIM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 10 {
			c.trim = n
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

func termWidth() int {
	if s, err := tsize.GetSize(); err == nil && s.Width > 0 {
		return s.Width
	}
	return 80
}

func main() {
	cfg, apiKey := loadConfig()
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "set TOROID_LLM_TOKEN to run (model is %s)\n", cfg.model)
		os.Exit(1)
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
	})
	if err != nil {
		return nil, err
	}

	// Tool call begins: compact one-liner, args trimmed to the configured width.
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			fmt.Printf("  %s⚙ %s%s%s %s\n", aBlue, aBold, p.Name, aReset, dimArgs(p.Args, cfg.trim))
		}
		return nil
	})
	// Tool call result: a short preview, or the error in red.
	k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			fmt.Printf("  %s└→%s %s\n", aGray, aReset, trimOneLine(p.Result, cfg.trim))
		}
		return nil
	})
	k.On(toroid.EventPostToolUseFailure, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			fmt.Printf("  %s└→ error: %s%s\n", aRed, trimOneLine(p.Error, cfg.trim), aReset)
		}
		return nil
	})
	// Reasoning/thinking, when enabled, shown dimmed inline.
	k.On(toroid.EventReasoning, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ReasoningPayload); ok {
			fmt.Print(aGray + aItalic + p.Text + aReset)
		}
		return nil
	})
	return k, nil
}

// ask drives one turn: run the prompt (blocking), then render the Markdown answer
// and the running cost. Tool activity streams live via the hooks above.
func ask(ctx context.Context, k *toroid.Kernel, cfg config, prompt string) {
	out, _, err := k.Run(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%srun error: %v%s\n", aRed, err, aReset)
		return
	}
	width := termWidth()
	fmt.Printf("\n%s%s ◂%s\n", aMagenta+aBold, shortModel(cfg.model), aReset)
	fmt.Print(renderMarkdown(out, width))
	fmt.Printf("%s  cost so far: $%.6f%s\n", aGray, k.RunningCostUSD(), aReset)
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
			aYellow, cfg.model, cfg.workdir, cfg.thinking, aReset)
	case "/help", "/?":
		printHelp()
	default:
		fmt.Printf("%sunknown command %q — try /help%s\n", aRed, line, aReset)
	}
	return false, false
}

func banner(cfg config) {
	fmt.Printf("%s┌─────────────────────────────────────────────%s\n", aCyan, aReset)
	fmt.Printf("%s│ toroid-repl%s  %s%s%s\n", aCyan+aBold, aReset, aGray, shortModel(cfg.model), aReset)
	fmt.Printf("%s│%s thinking=%s save=%v workdir=%s\n", aCyan, aReset, cfg.thinking, cfg.save, cfg.workdir)
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
config via flags: --save, --thinking (none|low|high), --no-colour
` + aReset)
}

// shortModel drops the provider prefix for a tidier label.
func shortModel(m string) string {
	if _, rest, ok := strings.Cut(m, "/"); ok {
		return rest
	}
	return m
}
