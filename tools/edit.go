package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

type EditArgs struct {
	FilePath string `json:"filePath" jsonschema:"description=The absolute path to the file to edit"`
	OldText  string `json:"oldText" jsonschema:"description=The exact text to replace"`
	NewText  string `json:"newText" jsonschema:"description=The text to replace it with"`
}

func NewEditTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("edit", desc, func(ctx context.Context, args EditArgs) (llm.ToolResult, error) {
		path := args.FilePath
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.WorkDir(), path)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}

		sContent := string(content)
		if !strings.Contains(sContent, args.OldText) {
			return llm.NewTextResult("Error: oldText not found in file"), nil
		}

		if strings.Count(sContent, args.OldText) > 1 {
			return llm.NewTextResult("Error: oldText found multiple times, please be more specific"), nil
		}

		newContent := strings.Replace(sContent, args.OldText, args.NewText, 1)
		if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}

		return llm.NewTextResult("File edited successfully."), nil
	})

	return &ToolDef{
		Name:        "edit",
		Description: desc,
		Template:    "edit.tool.tmpl",
		Handler:     h,
	}
}
