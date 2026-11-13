package tools

import (
	"context"
	"encoding/json"
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
	// Alias fields so the model can use the same field names as the edit tool.
	OldText string `json:"oldText,omitempty" jsonschema:"-"`
	NewText string `json:"newText,omitempty" jsonschema:"-"`
}

// MultiEditArgsRaw is the raw JSON shape for multiedit, used for custom unmarshaling
// so the tool can accept edits as either an array or a single object.
type MultiEditArgsRaw struct {
	FilePath       string          `json:"path"`
	LegacyFilePath string          `json:"filePath,omitempty"`
	Edits          json.RawMessage `json:"edits"`
}

func (a MultiEditArgsRaw) path() string {
	if a.FilePath != "" {
		return a.FilePath
	}
	return a.LegacyFilePath
}

func NewMultiEditTool(a Agent, desc string) *ToolDef {
	h := llm.RawTool("multiedit", desc, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":     map[string]any{"description": "Repository-relative path to modify; absolute only for files outside the workspace", "minLength": 1, "type": "string"},
			"filePath": map[string]any{"type": "string"},
			"edits": map[string]any{
				"description": "Ordered edits to apply. Can be a single edit object or an array of edits.",
				"minItems":    1,
				"oneOf": []any{
					map[string]any{"type": "array", "items": editSchema()},
					editSchema(),
				},
			},
		},
		"additionalProperties": false,
		"required":             []string{"path", "edits"},
	}, func(ctx context.Context, argumentsJSON string) (llm.ToolResult, error) {
		var raw MultiEditArgsRaw
		if err := json.Unmarshal([]byte(argumentsJSON), &raw); err != nil {
			return llm.NewErrorResult(fmt.Sprintf("Error: multiedit: invalid arguments (%v)", err)), nil
		}

		// Parse edits: accept a single object or an array.
		var edits []Edit
		if err := json.Unmarshal(raw.Edits, &edits); err != nil {
			var single Edit
			if err2 := json.Unmarshal(raw.Edits, &single); err2 != nil {
				return llm.NewErrorResult(fmt.Sprintf("Error: multiedit: edits must be an edit object or array of edits (%v)", err)), nil
			}
			edits = []Edit{single}
		}

		// Normalize aliases: oldText → oldString, newText → newString.
		for i := range edits {
			e := &edits[i]
			if e.OldString == "" && e.OldText != "" {
				e.OldString = e.OldText
			}
			if e.NewString == "" && e.NewText != "" {
				e.NewString = e.NewText
			}
		}

		if raw.path() == "" || len(edits) == 0 {
			return llm.NewErrorResult("Error: path and at least one edit are required"), nil
		}
		path := raw.path()
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.WorkDir(), path)
		}

		b, err := os.ReadFile(path)
		if err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}
		content := string(b)

		for i, e := range edits {
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

// editSchema returns the JSON schema for a single edit object.
func editSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"oldString":  map[string]any{"description": "The exact string to replace", "minLength": 1, "type": "string"},
			"newString":  map[string]any{"description": "The string to replace with", "type": "string"},
			"replaceAll": map[string]any{"description": "If true, replace all occurrences", "type": "boolean"},
			"oldText":    map[string]any{"description": "Alias for oldString (same as edit tool)", "type": "string"},
			"newText":    map[string]any{"description": "Alias for newString (same as edit tool)", "type": "string"},
		},
		"additionalProperties": false,
	}
}
