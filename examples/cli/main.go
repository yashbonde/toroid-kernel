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
//	flag --save            persist events/costs to the SQLite store (default on)
//	flag --no-save         disable persistence
//	flag --thinking        none | low | high      (default low)
//	flag --no-colour       disable all ANSI styling (default off)
//	flag --context-size    total context window size (0 = use kernel default)
//	flag --compact-buffer  tokens reserved before auto-compact triggers (0 = use kernel default)
//	flag --max-iter        max tool-call iterations per turn (default 1000)
//	flag --max-repeat-calls stop after N consecutive identical tool calls (0 or 1 disables the guard)
//	flag --smaller-model   cheaper model for compaction/subagents (empty = use primary)
//	flag --max-spend       max cumulative spend in USD (0 = unlimited)
//
//	export TOROID_LLM_TOKEN=your_api_key
//	go run ./examples/cli --model openai/gpt-4o --thinking high --save
//
// In-REPL commands: /help /cost /model /new /reset /clear /exit  (or Ctrl-D to quit).
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tsize "github.com/kopoli/go-terminal-size"
	toroid "github.com/yashbonde/toroid-kernel"
	"golang.org/x/sys/unix"
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

	// lastTurn is the most recent single-turn token usage, captured from
	// EventTurnCost so the per-turn footer reflects this turn (not the
	// session-accumulated totals carried in EventStop's UsagePayload).
	lastTurn toroid.Usage
}

// loadConfig reads env vars (model/token/iter/trim) and parses the flags
// (--model, --save, --no-save, --thinking, --no-colour, --run, --plain, --tokens). It
// returns the config plus the resolved API key. Flags win for the per-run
// toggles; env wins for the targeting knobs.
//
// A bare first positional argument is taken as the model id
// (`cli deepseek/deepseek-v4-flash-0731 --run '...'`): Go's flag package
// stops parsing at the first non-flag argument, so it is rewritten into
// --model before Parse.
func loadConfig() (config, string) {
	model := flag.String("model", "", "override TOROID_MODEL (provider/model)")
	save := flag.Bool("save", true, "persist events, costs and metadata to the SQLite store")
	noSave := flag.Bool("no-save", false, "disable persistence (overrides --save)")
	thinking := flag.String("thinking", "low", "thinking budget: none | low | high")
	noColour := flag.Bool("no-colour", false, "disable ANSI colour/styling")
	run := flag.String("run", "", "one-shot: run this prompt, emit NDJSON events, and exit (non-interactive)")
	plain := flag.Bool("plain", false, "with --run: print only the final assistant response as plain text, not the NDJSON event stream")
	tokens := flag.Bool("tokens", false, "with --run: include per-step Reasoning deltas in the NDJSON stream")

	// Context/compaction knobs
	contextSize := flag.Int("context-size", 0, "total context window size (0 = kernel default 200000)")
	compactBuffer := flag.Int("compact-buffer", 0, "tokens reserved below context-size before auto-compact fires (0 = kernel default 50000)")
	maxIterFlag := flag.Int("max-iter", 1000, "max tool-call iterations per turn (0 = use TOROID_MAX_ITER or kernel default 100)")
	maxRepeatCalls := flag.Int("max-repeat-calls", 0, "stop after N consecutive identical tool calls (0 or 1 = guard disabled)")
	smallerModel := flag.String("smaller-model", "", "cheaper model for compaction and subagents (empty = use primary model)")
	maxSpend := flag.Float64("max-spend", 0, "maximum cumulative transcript spend in USD (0 = unlimited)")
	maxTokens := flag.Int("max-tokens", 0, "max output tokens per llm-step (0 = provider default)")
	// Support a bare leading positional model: `cli <provider/model> …`. The
	// standard flag parser stops at the first non-flag argument, which would
	// leave the flags that follow unparsed; peel the positional off first.
	args := os.Args[1:]
	var positionalModel string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positionalModel = args[0]
		args = args[1:]
	}
	flag.CommandLine.Parse(args)

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

	// Flag --model overrides env TOROID_MODEL; a bare positional model id
	// (peeled off above) overrides the env default but loses to the flag.
	modelStr := envOr("TOROID_MODEL", "llmgateway/claude-haiku-4-5")
	if positionalModel != "" {
		modelStr = positionalModel
	}
	if *model != "" {
		modelStr = *model
	}

	c := config{
		model:    modelStr,
		workdir:  absWd,
		thinking: toroid.Thinking(*thinking),
		save:     *save && !*noSave,
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
	if len(os.Args) > 1 && os.Args[1] == "models" {
		if err := runModels(context.Background(), os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "trk models:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "sessions" {
		if err := runSessions(context.Background(), os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "trk sessions:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "watch" {
		if err := runWatch(os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "trk watch:", err)
			os.Exit(1)
		}
		return
	}

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

	if err := runTUI(cfg, apiKey); err != nil {
		fmt.Fprintln(os.Stderr, "cli:", err)
		os.Exit(1)
	}
}

// newKernel wires a kernel plus all the pretty-printing event hooks. Recreated on
// /reset to start a fresh session.
func newKernel(ctx context.Context, cfg *config, apiKey string, emit func(string)) (*toroid.Kernel, error) {
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
		return nil, err
	}
	if emit == nil {
		emit = func(s string) { fmt.Print(s) }
	}

	// Tool activity is rendered by the consuming front-end (the TUI animates a
	// live "… working" line; --run's one-shot prints box art via
	// renderToolCall/toolResultLine) rather than via this shared emit path, so
	// it is wired up at the call site instead of here.
	k.On(toroid.EventReasoning, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ReasoningPayload); ok {
			if !reasoningActive {
				reasoningActive = true
				emit(aGray + strings.Repeat("—", termWidth()) + aReset + "\n")
			}
			emit(aGray + aItalic + p.Text + aReset)
			reasoningNeedsNewline = !strings.HasSuffix(p.Text, "\n")
		}
		return nil
	})
	// Capture per-turn usage for the turn footer. TurnCompleted carries the
	// turn's own usage directly (what a separate TurnCost event used to
	// carry); EventStop's UsagePayload is session-accumulated, so keep this
	// turn's own numbers here instead.
	k.On(toroid.EventTurnCompleted, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TurnPayload); ok {
			cfg.lastTurn = p.TurnUsage
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
	defer cancel()

	out, _, err := k.Run(ctx, prompt)
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
	fmt.Println()
}

// renderToolCall draws a tool invocation as a compact box header:
//
//	┌─ bash  git ls-files | wc -l
//	└─ done · 2 lines · 4 bytes
//
// The header shows the operation and its meaningful target; its matching
// bottom edge (toolResultLine / toolErrorLine) reports shape/status. The
// whole thing is width-bounded so a chatty tool never eats the screen.
// toolCallLabel renders a tool call as a single, width-bounded display line
// ("bash  git ls-files | wc -l") without any box art. The TUI uses it for its
// live animated tool rows; renderToolCall/toolResultLine keep the box form for
// the print-only --run path.
func toolCallLabel(name, args, workDir string) string {
	summary := toolCallArgSummary(name, args, workDir)
	if summary == "" {
		return name
	}
	return name + "  " + summary
}

func renderToolCall(name, args, workDir string, width int) string {
	summary := toolCallArgSummary(name, args, workDir)
	// Leave room for the box decorations and the separating space.
	if max := width - len([]rune(name)) - 6; max > 10 {
		if runes := []rune(summary); len(runes) > max {
			summary = string(runes[:max-1]) + "…"
		}
	}
	if summary == "" {
		return fmt.Sprintf("%s┌─%s %s%s%s\n", aGray, aReset, aBold+aCyan, name, aReset)
	}
	return fmt.Sprintf("%s┌─%s %s%s%s  %s\n", aGray, aReset, aBold+aCyan, name, aReset, summary)
}

func toolCallArgSummary(name, args, workDir string) string {
	var summary string

	switch name {
	case "read":
		// Extract path, offset, limit
		path := extractJSONField(args, "path")
		if path == "" {
			path = extractJSONField(args, "filePath")
		}
		offset := extractJSONInt(args, "offset")
		limit := extractJSONInt(args, "limit")
		if offset > 0 || limit > 0 {
			relPath := makeRelative(path, workDir)
			if limit > 0 {
				summary = fmt.Sprintf("%s [%d:%d]", relPath, offset, offset+limit)
			} else {
				summary = fmt.Sprintf("%s [%d:]", relPath, offset)
			}
		} else {
			summary = makeRelative(path, workDir)
		}
	case "write":
		path := extractJSONField(args, "path")
		summary = makeRelative(path, workDir)
	case "edit":
		path := extractJSONField(args, "path")
		if path == "" {
			path = extractJSONField(args, "filePath")
		}
		summary = makeRelative(path, workDir)
	case "multiedit":
		path := extractJSONField(args, "path")
		if path == "" {
			path = extractJSONField(args, "filePath")
		}
		summary = makeRelative(path, workDir)
	case "bash":
		command := extractJSONField(args, "command")
		summary = compactToolText(command, 500)
	case "glob":
		pattern := extractJSONField(args, "pattern")
		summary = compactToolText(pattern, 500)
	case "grep":
		pattern := extractJSONField(args, "pattern")
		path := extractJSONField(args, "path")
		if path != "" {
			summary = fmt.Sprintf("%s in %s", compactToolText(pattern, 250), makeRelative(path, workDir))
		} else {
			summary = compactToolText(pattern, 500)
		}
	case "task":
		description := extractJSONField(args, "description")
		summary = compactToolText(description, 500)
	case "subagent", "subagent_async":
		description := extractJSONField(args, "task")
		if description == "" {
			description = extractJSONField(args, "description")
		}
		summary = compactToolText(description, 500)
	case "skill":
		path := extractJSONField(args, "path")
		summary = makeRelative(path, workDir)
	case "mcp":
		method := extractJSONField(args, "method")
		summary = compactToolText(method, 500)
	default:
		summary = compactToolText(args, 500)
	}

	return strings.TrimSpace(summary)
}

// toolResultLine renders the bottom edge of a call box: a shape/status summary
// ("done · 3 lines · 73 bytes") rather than an arbitrary result preview.
func toolResultLine(result string) string {
	return fmt.Sprintf("%s└─ %s%s%s\n", aGray, aDim, toolResultSummary(result), aReset)
}

// toolErrorLine renders the bottom edge of a call box for a failed tool,
// highlighting the compacted error text.
func toolErrorLine(errText string) string {
	return fmt.Sprintf("%s└─ %s⨯ %s%s\n", aGray, aRed, compactToolText(errText, 400), aReset)
}

func compactToolText(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	runes := []rune(s)
	if max > 0 && len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return s
}

func toolResultSummary(result string) string {
	result = strings.TrimSpace(result)
	if result == "" {
		return "done"
	}
	lines := strings.Count(result, "\n") + 1
	bytes := len(result)
	if lines == 1 {
		return fmt.Sprintf("done · %d bytes", bytes)
	}
	return fmt.Sprintf("done · %d lines · %d bytes", lines, bytes)
}

// extractJSONField extracts a string field from a simple JSON object.
func extractJSONField(jsonStr, field string) string {
	// Look for "field":"value" pattern
	search := fmt.Sprintf(`"%s":`, field)
	idx := strings.Index(jsonStr, search)
	if idx < 0 {
		return ""
	}
	idx += len(search)
	// Skip whitespace
	for idx < len(jsonStr) && (jsonStr[idx] == ' ' || jsonStr[idx] == '\t') {
		idx++
	}
	if idx >= len(jsonStr) || jsonStr[idx] != '"' {
		return ""
	}
	idx++ // skip opening quote
	start := idx
	for idx < len(jsonStr) && jsonStr[idx] != '"' {
		// Handle escaped quotes
		if jsonStr[idx] == '\\' && idx+1 < len(jsonStr) {
			idx += 2
		} else {
			idx++
		}
	}
	if idx > start {
		return jsonStr[start:idx]
	}
	return ""
}

// extractJSONInt extracts an integer field from a simple JSON object.
func extractJSONInt(jsonStr, field string) int {
	search := fmt.Sprintf(`"%s":`, field)
	idx := strings.Index(jsonStr, search)
	if idx < 0 {
		return 0
	}
	idx += len(search)
	// Skip whitespace
	for idx < len(jsonStr) && (jsonStr[idx] == ' ' || jsonStr[idx] == '\t') {
		idx++
	}
	if idx >= len(jsonStr) {
		return 0
	}
	start := idx
	for idx < len(jsonStr) && (jsonStr[idx] >= '0' && jsonStr[idx] <= '9') {
		idx++
	}
	if idx > start {
		val, _ := strconv.Atoi(jsonStr[start:idx])
		return val
	}
	return 0
}

// makeRelative converts an absolute path to a relative path from workDir.
func makeRelative(path, workDir string) string {
	if path == "" {
		return ""
	}
	// First, try to make it relative to workDir
	rel, err := filepath.Rel(workDir, path)
	if err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	// If it's outside workDir, return just the filename
	return filepath.Base(path)
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
	case "/delegate":
		// Handled at the input loop before handleCommand; provide a fallback help.
		fmt.Println(aGreen + "Type /delegate <task> to dispatch a task to another agent" + aReset)
	default:
		fmt.Printf("%sunknown command %q — try /help%s\n", aRed, line, aReset)
	}
	return false, false
}

func banner(cfg config) {
	fmt.Printf("%s┌─────────────────────────────────────────────%s\n", aCyan, aReset)
	fmt.Printf("%s│ toroid repl%s  %s%s%s\n", aCyan+aBold, aReset, aGray, shortModel(cfg.model), aReset)
	fmt.Printf("%s│%s thinking=%s save=%v workdir=%s\n", aCyan, aReset, cfg.thinking, cfg.save, displayWorkdir(cfg.workdir))
	fmt.Printf("%s└ %sEnter=send  Shift+Enter=newline  multiline paste supported  /help%s\n", aCyan, aGray, aReset)
}

func printHelp() {
	fmt.Print(aGray + `commands:
  /help        show this help
  /cost        running cost in USD for this session
  /model       show active model & config
  /reset       start a fresh session (clears history)
  /clear       clear the screen
  /delegate    dispatch a task to another agent (Slack, Codex, or trk)
  /exit        quit (or Ctrl-D)

input:
  Enter            send
  Shift+Enter      new line
  Multiline paste  preserved as one message
  Ctrl-C           interrupt current input
  Ctrl-D           exit (empty input)

config via env:   TOROID_MODEL, TOROID_LLM_TOKEN, TOROID_MAX_ITER, TOROID_TRIM
config via flags: --model, --save, --no-save, --thinking (none|low|high), --no-colour
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

// runLineMode is the fallback when cbreak mode isn't available.
func runLineMode(ctx context.Context, k *toroid.Kernel, cfg config) {
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

		if strings.HasPrefix(line, "/") {
			if strings.HasPrefix(line, "/delegate") {
				line = strings.TrimSpace(strings.TrimPrefix(line, "/delegate"))
				fmt.Printf("%s🛟 delegate identified%s\n", aGreen, aReset)
				out, err := delegate(k, line)
				if err != nil {
					fmt.Printf("%sdelegation error: %v%s\n", aRed, err, aReset)
				} else {
					fmt.Println(out)
				}
				continue
			}
			quit, reset := handleCommand(line, k, cfg)
			if quit {
				break
			}
			if reset {
				return // caller will recreate kernel
			}
			continue
		}

		ask(ctx, k, cfg, line)
	}
}

var bracketedPasteEnd = []byte("\x1b[201~")

// readEscapeSequence reads one complete CSI/Alt sequence after ESC was read.
func readEscapeSequence(fd int) []byte {
	seq := []byte{27}
	for {
		rfds := &unix.FdSet{}
		rfds.Set(fd)
		tv := &unix.Timeval{Sec: 0, Usec: 30000}
		n, _ := unix.Select(fd+1, rfds, nil, nil, tv)
		if n == 0 {
			return seq
		}
		b := []byte{0}
		nr, _ := unix.Read(fd, b)
		if nr == 0 {
			return seq
		}
		seq = append(seq, b[0])
		// The '[' introduces CSI; it is not itself the final byte.
		if len(seq) > 2 && b[0] >= 0x40 && b[0] <= 0x7e {
			return seq
		}
		if len(seq) == 2 && seq[1] != '[' {
			return seq
		}
	}
}

func readBracketedPaste(fd int) ([]byte, error) {
	var pasted []byte
	for {
		b := []byte{0}
		n, err := unix.Read(fd, b)
		if err != nil || n == 0 {
			return pasted, err
		}
		pasted = append(pasted, b[0])
		if len(pasted) >= len(bracketedPasteEnd) && bytes.Equal(pasted[len(pasted)-len(bracketedPasteEnd):], bracketedPasteEnd) {
			return pasted[:len(pasted)-len(bracketedPasteEnd)], nil
		}
	}
}

func modifiedEnter(seq []byte) (modifier string, ok bool) {
	s := string(seq)
	// CSI-u keyboard protocol: ESC [ 13 ; modifier u
	if strings.HasPrefix(s, "\x1b[13;") && strings.HasSuffix(s, "u") {
		return strings.TrimSuffix(strings.TrimPrefix(s, "\x1b[13;"), "u"), true
	}
	// xterm modifyOtherKeys level 2: ESC [ 27 ; modifier ; 13 ~
	if strings.HasPrefix(s, "\x1b[27;") && strings.HasSuffix(s, ";13~") {
		return strings.TrimSuffix(strings.TrimPrefix(s, "\x1b[27;"), ";13~"), true
	}
	return "", false
}

func displayInputText(text string) {
	fmt.Print(strings.ReplaceAll(text, "\n", "\n"+strings.Repeat(" ", 8)))
}

// runCBreakMode reads chat-style input: Enter submits, Shift+Enter adds a
// newline, and bracketed paste adds its complete payload without submitting.
func runCBreakMode(ctx context.Context, k *toroid.Kernel, cfg config, fd int) {
	var buf []byte
	promptPrinted := false

	// modifyOtherKeys (CSI > 4 ; 2 m) emits sequences like:
	//   Enter:         \x1b[13;5u  (Ctrl+Enter)
	//   Enter:         \x1b[13;2u  (Shift+Enter)
	//   Enter:         \x1b[13;6u  (Ctrl+Shift+Enter)
	//   Normal Enter:  \n or \r
	// We'll parse these escape sequences to detect modifier+Enter.

	for {
		if !promptPrinted {
			fmt.Print(aGreen + aBold + "\nyou ▸ " + aReset)
			promptPrinted = true
		}

		// Read one byte at a time in cbreak mode
		b := make([]byte, 1)
		n, err := unix.Read(fd, b)
		if err != nil || n == 0 {
			if err == unix.EINTR {
				continue
			}
			fmt.Println()
			break
		}

		ch := b[0]

		// Handle Ctrl-C (SIGINT) - interrupt current input
		if ch == 3 {
			fmt.Println(aYellow + "\n[interrupted]" + aReset)
			buf = nil
			promptPrinted = false
			continue
		}

		// Handle Ctrl-D (EOF) - exit
		if ch == 4 {
			if len(buf) == 0 {
				fmt.Println()
				break
			}
			// If there's content, treat as submit
			ch = '\n'
		}

		// Handle escape sequences for bracketed paste and modified Enter.
		if ch == 27 { // ESC
			seq := readEscapeSequence(fd)
			if bytes.Equal(seq, []byte("\x1b[200~")) {
				pasted, err := readBracketedPaste(fd)
				if err != nil {
					fmt.Println()
					return
				}
				text := strings.ReplaceAll(strings.ReplaceAll(string(pasted), "\r\n", "\n"), "\r", "\n")
				buf = append(buf, text...)
				displayInputText(text)
				continue
			}
			if modifier, ok := modifiedEnter(seq); ok {
				if modifier == "2" {
					buf = append(buf, '\n')
					fmt.Print("\n" + strings.Repeat(" ", 8))
					continue
				}
				// Other modified Enter variants retain send behavior.
				ch = '\n'
			} else {
				// Other escape sequences (arrow keys, etc.): add to buffer.
				buf = append(buf, seq...)
				fmt.Print(string(seq))
				continue
			}
		}

		// Plain Enter sends the complete buffered message.
		if ch == '\n' || ch == '\r' {
			line := strings.TrimSpace(string(buf))
			if line != "" {
				buf = nil
				promptPrinted = false

				if strings.HasPrefix(line, "/") {
					if strings.HasPrefix(line, "/delegate") {
						line = strings.TrimSpace(strings.TrimPrefix(line, "/delegate"))
						fmt.Printf("%s🛟 delegate identified%s\n", aGreen, aReset)
						out, err := delegate(k, line)
						if err != nil {
							fmt.Printf("%sdelegation error: %v%s\n", aRed, err, aReset)
						} else {
							fmt.Println(out)
						}
						continue
					}
					quit, reset := handleCommand(line, k, cfg)
					if quit {
						return
					}
					if reset {
						return // caller will recreate kernel
					}
					continue
				}

				ask(ctx, k, cfg, line)
				continue
			}

			// Ignore Enter on an empty input.
			continue
		}

		// Handle backspace/delete
		if ch == 127 || ch == 8 { // DEL or BS
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Print("\b \b") // erase char visually
			}
			continue
		}

		// Printable character
		if ch >= 32 && ch < 127 {
			buf = append(buf, ch)
			fmt.Printf("%c", ch)
			continue
		}

		// Other control chars: ignore but keep in buffer for completeness
		buf = append(buf, ch)
	}
}
