package tools

import (
	"context"
	"os/exec"

	"github.com/yashbonde/toroid-kernel/llm"
)

type BashArgs struct {
	Command string `json:"command" jsonschema:"description=The bash command to execute"`
}

func NewBashTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("bash", desc, func(ctx context.Context, args BashArgs) (llm.ToolResult, error) {
		cmd := exec.CommandContext(ctx, "bash", "-c", args.Command)
		cmd.Dir = a.WorkDir()
		out, err := cmd.CombinedOutput()
		outStr := TruncateToolOutput(string(out))
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
