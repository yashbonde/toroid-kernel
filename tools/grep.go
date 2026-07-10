package tools

import (
	"context"
	"os/exec"

	"github.com/yashbonde/toroid-kernel/llm"
)

type GrepArgs struct {
	Pattern string `json:"pattern" jsonschema:"description=The regular expression pattern to search for"`
	Path    string `json:"path" jsonschema:"description=The path to search in (defaults to current working directory),default=."`
}

func NewGrepTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("grep", desc, func(ctx context.Context, args GrepArgs) (llm.ToolResult, error) {
		path := args.Path
		if path == "" {
			path = "."
		}

		cmd := exec.CommandContext(ctx, "grep", "-r", "-n", "-C", "2", args.Pattern, path)
		cmd.Dir = a.WorkDir()
		out, err := cmd.CombinedOutput()
		if err != nil && len(out) == 0 {
			return llm.NewTextResult("No matches found."), nil
		}

		return llm.NewTextResult(TruncateToolOutput(string(out))), nil
	})

	return &ToolDef{
		Name:        "grep",
		Description: desc,
		Template:    "grep.tool.tmpl",
		Handler:     h,
	}
}
