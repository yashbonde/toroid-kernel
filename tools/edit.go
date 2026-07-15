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
	FilePath       string `json:"path" jsonschema:"description=Repository-relative path to edit; absolute only for files outside the workspace,minLength=1"`
	LegacyFilePath string `json:"filePath,omitempty" jsonschema:"-"`
	OldText        string `json:"oldText" jsonschema:"description=The exact text to replace,minLength=1"`
	NewText        string `json:"newText" jsonschema:"description=The text to replace it with"`
}

func (a EditArgs) path() string {
	if a.FilePath != "" {
		return a.FilePath
	}
	return a.LegacyFilePath
}

func NewEditTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("edit", desc, func(ctx context.Context, args EditArgs) (llm.ToolResult, error) {
		if args.path() == "" || args.OldText == "" {
			return llm.NewErrorResult("Error: path and oldText are required"), nil
		}
		path := args.path()
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
		Handler:     h,
	}
}
