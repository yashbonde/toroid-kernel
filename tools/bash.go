package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/yashbonde/toroid-kernel/llm"
)

// DefaultBashTimeout bounds any single command so a hang (interactive editor,
// stalled network call, infinite loop) can never block the kernel forever.
// Overridable per call via BashArgs.Timeout.
const DefaultBashTimeout = 120 * time.Second

// nonInteractiveEnv neutralizes commands that would otherwise spawn an
// interactive editor or pager and wait on a terminal that isn't there:
// `git commit` (no -m), `git rebase -i`, `crontab -e`, `git config --edit`, apt,
// etc. With EDITOR=true the editor step exits 0 immediately; GIT_TERMINAL_PROMPT=0
// makes credential prompts fail fast instead of blocking; PAGER=cat stops a
// pager from waiting for `q`.
var nonInteractiveEnv = []string{
	"GIT_EDITOR=true",
	"EDITOR=true",
	"VISUAL=true",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_PAGER=cat",
	"PAGER=cat",
	"DEBIAN_FRONTEND=noninteractive",
}

type BashArgs struct {
	Command string `json:"command" jsonschema:"description=The bash command to execute,minLength=1"`
	// Timeout in seconds for this command. 0 uses DefaultBashTimeout (120s).
	Timeout int `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds (default 120),minimum=1,maximum=600"`
	// Background runs the command fire-and-forget: the call returns a task id
	// and log path immediately; on exit the completion notice is injected into
	// the conversation like a background-subagent result.
	Background bool `json:"background,omitempty" jsonschema:"description=Run detached in the background; returns a task id and log path immediately, completion notice arrives later"`
}

// BackgroundNotifier is an optional capability the bash tool uses to deliver
// background-command completion through the kernel's queue + wake-up path
// (the same route a background subagent takes). The kernel implements it;
// tests and other Agent implementations may not.
type BackgroundNotifier interface {
	EnqueueBackgroundNotice(id, notice string)
}

// rtkReadCommands are read-only command prefixes that rtk (a token-optimizing
// CLI proxy) knows how to compress. When rtk is on PATH, simple invocations of
// these are transparently rewritten to "rtk <cmd>" so their output costs a
// fraction of the tokens.
var rtkReadCommands = []string{
	"git status", "git diff", "git log", "git show", "git branch",
	"ls", "cat", "head", "tail", "grep", "rg", "find", "wc", "tree", "du", "df", "ps",
}

// rewriteWithRTK prefixes cmd with "rtk " when it is a simple (un-chained)
// invocation of a known read-only command. Compound commands (pipes, &&, ;,
// redirection, substitution) are left untouched.
func rewriteWithRTK(cmd string) string {
	if strings.ContainsAny(cmd, "|&;><`$") {
		return cmd
	}
	trimmed := strings.TrimSpace(cmd)
	for _, p := range rtkReadCommands {
		if trimmed == p || strings.HasPrefix(trimmed, p+" ") {
			return "rtk " + trimmed
		}
	}
	return cmd
}

func NewBashTool(a Agent, desc string) *ToolDef {
	_, rtkErr := exec.LookPath("rtk")
	rtkAvailable := rtkErr == nil
	if rtkAvailable {
		desc += "\nSimple read-only commands (git status/diff/log, ls, cat, grep, find, …) are automatically run through rtk, which compresses their output."
	}

	h := llm.NewTool("bash", desc, func(ctx context.Context, args BashArgs) (llm.ToolResult, error) {
		if args.Background {
			return runBackgroundCommand(a, args.Command)
		}
		if args.Timeout < 0 || args.Timeout > 600 {
			return llm.NewErrorResult("Error: timeout must be between 1 and 600 seconds when set"), nil
		}
		command := args.Command
		if rtkAvailable {
			command = rewriteWithRTK(command)
		}

		// Per-command timeout so nothing can hang the kernel indefinitely.
		timeout := DefaultBashTimeout
		if args.Timeout > 0 {
			timeout = time.Duration(args.Timeout) * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = a.WorkDir()

		// Non-interactive environment: an editor/pager/prompt can never block.
		cmd.Env = append(os.Environ(), nonInteractiveEnv...)

		// Empty stdin: anything reading stdin (cat, read, python) gets EOF
		// immediately instead of waiting for input that will never arrive.
		cmd.Stdin = nil

		// Own process group so a timeout kills the whole tree — bash and any
		// grandchildren it spawned (e.g. an editor that opened /dev/tty
		// directly) — not just the direct child, which could orphan them.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return os.ErrProcessDone
		}
		cmd.WaitDelay = 2 * time.Second

		out, err := cmd.CombinedOutput()
		outStr := TruncateToolOutput(a, string(out))
		if ctx.Err() == context.DeadlineExceeded {
			return llm.NewTextResult(outStr + fmt.Sprintf("\nError: command timed out after %s (killed). Re-run with a larger \"timeout\" if it legitimately needs longer, or avoid commands that wait on interactive input.", timeout)), nil
		}
		if err != nil {
			return llm.NewTextResult(outStr + "\nError: " + err.Error()), nil
		}
		return llm.NewTextResult(outStr), nil
	})

	return &ToolDef{
		Name:        "bash",
		Description: desc,
		Handler:     h,
	}
}

// runBackgroundCommand starts command detached under bash, in the agent's work
// directory, writing stdout+stderr to a log file. It returns the task id, log
// path, and PID immediately; a waiter goroutine reports the exit status through
// the kernel's background-notice queue (no LLM involvement). If the Agent does
// not support background notifications the command still runs — the caller can
// read the log file — but no completion notice is delivered.
func runBackgroundCommand(a Agent, command string) (llm.ToolResult, error) {
	if !a.BackgroundEnabled() {
		return llm.NewErrorResult("Error: background commands are not enabled (set IncludeSubagentTools in the kernel config)"), nil
	}
	id := fmt.Sprintf("bgcmd-%d", time.Now().UnixNano()%1e6)
	logPath := fmt.Sprintf("/tmp/toroid-bgcmd-%s.log", id)

	// Detached run: output to the log file, own process group so it survives
	// and can be killed as a tree later. The command is passed verbatim to
	// bash -c so compound commands (pipes, &&) parse naturally.
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = a.WorkDir()
	logFile, err := os.Create(logPath)
	if err != nil {
		return llm.NewErrorResult("Error: cannot create log file: " + err.Error()), nil
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), nonInteractiveEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return llm.NewErrorResult("Error: failed to start: " + err.Error()), nil
	}
	pid := cmd.Process.Pid

	// Waiter: no model calls, only completion bookkeeping. Detached from the
	// tool-call context on purpose — the command must survive the call.
	go func() {
		err := cmd.Wait()
		status := "completed"
		if err != nil {
			status = fmt.Sprintf("failed (%v)", err)
		}
		if n, ok := a.(BackgroundNotifier); ok {
			n.EnqueueBackgroundNotice(id, fmt.Sprintf("background command %s %s. Log: %s (last lines: run `tail -50 %s`)", id, status, logPath, logPath))
		}
	}()

	return llm.NewTextResult(fmt.Sprintf("Started background command %s (pid %d).\nLog: %s\nRead output with: tail -n 50 %s", id, pid, logPath, logPath)), nil
}
