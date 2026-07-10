package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yashbonde/toroid-kernel/llm"
)

type WriteArgs struct {
	Path    string `json:"path" jsonschema:"description=The absolute path to the file to write"`
	Content string `json:"content" jsonschema:"description=The complete content to write to the file"`
}

func NewWriteTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("write", desc, func(ctx context.Context, args WriteArgs) (llm.ToolResult, error) {
		path := args.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.WorkDir(), path)
		}

		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}

		if err := os.WriteFile(path, []byte(args.Content), 0644); err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}

		return llm.NewTextResult("File written successfully."), nil
	})

	return &ToolDef{
		Name:        "write",
		Description: desc,
		Template:    "write.tool.tmpl",
		Handler:     h,
	}
}
