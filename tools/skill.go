package tools

import (
	"context"
	"fmt"
	"os"

	"charm.land/fantasy"
)

type SkillArgs struct {
	Path string `json:"path" jsonschema:"description=Path to the skill file to load in full (as listed in the Skills section of the system prompt, or named directly by the user)"`
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
	fTool := fantasy.NewAgentTool("skill", desc, func(ctx context.Context, args SkillArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if err := args.Validate(); err != nil {
			return fantasy.NewTextErrorResponse("Error: " + err.Error()), nil
		}

		path := ResolvePath(args.Path, a.WorkDir())
		b, err := os.ReadFile(path)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("Error: %v", err)), nil
		}
		return fantasy.NewTextResponse(TruncateToolOutput(string(b))), nil
	})

	return &ToolDef{
		Name:        "skill",
		Description: desc,
		Template:    "skill.tool.tmpl",
		AgentTool:   fTool,
	}
}
