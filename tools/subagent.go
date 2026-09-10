package tools

import (
	"context"

	"github.com/yashbonde/toroid-kernel/llm"
)

type SubagentArgs struct {
	Task  string `json:"task" jsonschema:"description=Self-contained subtask with goal paths constraints and expected result,minLength=1"`
	Model string `json:"model,omitempty" jsonschema:"description=Model id to run this subtask on. Omit unless the user named a specific model."`
}

// SubagentRunner is the optional extension of Agent for agents that can run a
// subagent on a caller-chosen model. Agents that do not implement it simply
// ignore the model argument and run on their default.
type SubagentRunner interface {
	RunSubagentModel(ctx context.Context, task, model string) (string, error)
}

// NewSubagentTool runs a subagent synchronously and returns its output.
func NewSubagentTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("subagent", desc, func(ctx context.Context, args SubagentArgs) (llm.ToolResult, error) {
		var output string
		var err error
		if r, ok := a.(SubagentRunner); ok {
			output, err = r.RunSubagentModel(ctx, args.Task, args.Model)
		} else {
			output, err = a.RunSubagent(ctx, args.Task)
		}
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
		Handler:     h,
	}
}
