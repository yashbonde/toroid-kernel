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
