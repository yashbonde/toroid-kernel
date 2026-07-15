package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

type Edit struct {
	OldString  string `json:"oldString" jsonschema:"description=The exact string to replace,minLength=1"`
	NewString  string `json:"newString" jsonschema:"description=The string to replace with"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"description=If true, replace all occurrences"`
}

type MultiEditArgs struct {
	FilePath       string `json:"path" jsonschema:"description=Repository-relative path to modify; absolute only for files outside the workspace,minLength=1"`
	LegacyFilePath string `json:"filePath,omitempty" jsonschema:"-"`
	Edits          []Edit `json:"edits" jsonschema:"description=Ordered edits to apply,minItems=1"`
}

func (a MultiEditArgs) path() string {
	if a.FilePath != "" {
		return a.FilePath
	}
	return a.LegacyFilePath
}

func NewMultiEditTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("multiedit", desc, func(ctx context.Context, args MultiEditArgs) (llm.ToolResult, error) {
		if args.path() == "" || len(args.Edits) == 0 {
			return llm.NewErrorResult("Error: path and at least one edit are required"), nil
		}
		path := args.path()
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.WorkDir(), path)
		}

		b, err := os.ReadFile(path)
		if err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}
		content := string(b)

		for i, e := range args.Edits {
			if e.OldString == "" {
				return llm.NewErrorResult(fmt.Sprintf("Error: edit %d: oldString is required", i)), nil
			}
			oldStr := e.OldString
			newStr := e.NewString
			replaceAll := e.ReplaceAll

			count := strings.Count(content, oldStr)
			if count == 0 {
				return llm.NewTextResult(fmt.Sprintf("Error: edit %d: oldString not found", i)), nil
			}
			if !replaceAll && count > 1 {
				return llm.NewTextResult(fmt.Sprintf("Error: edit %d: found multiple matches for oldString", i)), nil
			}

			n := 1
			if replaceAll {
				n = -1
			}
			content = strings.Replace(content, oldStr, newStr, n)
		}

		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}

		return llm.NewTextResult("Multiple edits applied successfully."), nil
	})

	return &ToolDef{
		Name:        "multiedit",
		Description: desc,
		Handler:     h,
	}
}
