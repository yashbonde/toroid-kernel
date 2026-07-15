package tools

import (
	"context"
	"fmt"
	"os"

	"github.com/yashbonde/toroid-kernel/llm"
)

type SkillArgs struct {
	Path string `json:"path" jsonschema:"description=Exact listed path of the skill to load,minLength=1"`
}

func (a SkillArgs) Validate() error {
	if a.Path == "" {
		return fmt.Errorf("path is required")
	}
	return nil
}

// NewSkillTool loads the full contents of a skill file on demand. Only a
// skill's name and description are known up front (injected into the system
// prompt at startup); the full instructions are read from disk — and so only
// paid for in context — when this tool is actually called.
func NewSkillTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("skill", desc, func(ctx context.Context, args SkillArgs) (llm.ToolResult, error) {
		if err := args.Validate(); err != nil {
			return llm.NewErrorResult("Error: " + err.Error()), nil
		}

		path := ResolvePath(args.Path, a.WorkDir())
		b, err := os.ReadFile(path)
		if err != nil {
			return llm.NewErrorResult(fmt.Sprintf("Error: %v", err)), nil
		}
		return llm.NewTextResult(TruncateToolOutput(a, string(b))), nil
	})

	return &ToolDef{
		Name:        "skill",
		Description: desc,
		Handler:     h,
	}
}
