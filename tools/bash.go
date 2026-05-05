package tools

import (
	"context"
	"os/exec"

	"charm.land/fantasy"
)

type BashArgs struct {
	Command string `json:"command" jsonschema:"description=The bash command to execute"`
}

func NewBashTool(a Agent, desc string) *ToolDef {
	fTool := fantasy.NewAgentTool("bash", desc, func(ctx context.Context, args BashArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		cmd := exec.CommandContext(ctx, "bash", "-c", args.Command)
		cmd.Dir = a.WorkDir()
		const maxOutputChars = 20_000 // ~5k tokens
		out, err := cmd.CombinedOutput()
		outStr := string(out)
		if len(outStr) > maxOutputChars {
			outStr = outStr[:maxOutputChars] + "\n… [truncated]"
		}
		if err != nil {
			return fantasy.ToolResponse{Type: "text", Content: outStr + "\nError: " + err.Error()}, nil
		}
		return fantasy.ToolResponse{Type: "text", Content: outStr}, nil
	})

	return &ToolDef{
		Name:        "bash",
		Description: desc,
		Template:    "bash.tool.tmpl",
		AgentTool:   fTool,
	}
}
