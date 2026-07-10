package tools

import (
	"context"
	"os/exec"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

type BashArgs struct {
	Command string `json:"command" jsonschema:"description=The bash command to execute"`
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
		command := args.Command
		if rtkAvailable {
			command = rewriteWithRTK(command)
		}
		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = a.WorkDir()
		out, err := cmd.CombinedOutput()
		outStr := TruncateToolOutput(a, string(out))
		if err != nil {
			return llm.NewTextResult(outStr + "\nError: " + err.Error()), nil
		}
		return llm.NewTextResult(outStr), nil
	})

	return &ToolDef{
		Name:        "bash",
		Description: desc,
		Template:    "bash.tool.tmpl",
		Handler:     h,
	}
}
