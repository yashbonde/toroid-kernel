package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

type GlobArgs struct {
	Pattern string `json:"pattern" jsonschema:"description=The glob pattern to match (e.g. '**/*.go')"`
}

func NewGlobTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("glob", desc, func(ctx context.Context, args GlobArgs) (llm.ToolResult, error) {
		cmd := exec.CommandContext(ctx, "find", ".", "-name", args.Pattern)
		cmd.Dir = a.WorkDir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return llm.NewTextResult(string(out) + "\nError: " + err.Error()), nil
		}

		lines := strings.Split(string(out), "\n")
		var filtered []string
		for _, l := range lines {
			if l != "" {
				filtered = append(filtered, l)
			}
		}

		content := TruncateToolOutput(fmt.Sprintf("<matches>\n%s\n</matches>", strings.Join(filtered, "\n")))
		return llm.NewTextResult(content), nil
	})

	return &ToolDef{
		Name:        "glob",
		Description: desc,
		Template:    "glob.tool.tmpl",
		Handler:     h,
	}
}
