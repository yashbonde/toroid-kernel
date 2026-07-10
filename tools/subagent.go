package tools

import (
	"context"

	"github.com/yashbonde/toroid-kernel/llm"
)

type SubagentArgs struct {
	Task string `json:"task" jsonschema:"description=Full description of the subtask for the subagent to handle"`
}

// NewSubagentTool runs a subagent synchronously and returns its output.
func NewSubagentTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("subagent", desc, func(ctx context.Context, args SubagentArgs) (llm.ToolResult, error) {
		output, err := a.RunSubagent(ctx, args.Task)
		if err != nil {
			return llm.ToolResult{}, err
		}
		// Cap like every other tool: a verbose subagent transcript would
		// otherwise ride in the parent's prompt at full size every turn.
		return llm.NewTextResult(TruncateToolOutput(a, output)), nil
	})

	return &ToolDef{
		Name:        "subagent",
		Description: desc,
		Template:    "subagent.tool.tmpl",
		Handler:     h,
	}
}
