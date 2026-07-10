package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

type LsArgs struct {
	Dir string `json:"dir" jsonschema:"description=The absolute path to the directory to list (defaults to current working directory),default=."`
}

func NewLsTool(a Agent, desc string) *ToolDef {
	h := llm.NewTool("ls", desc, func(ctx context.Context, args LsArgs) (llm.ToolResult, error) {
		path := args.Dir
		if path == "" {
			path = "."
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.WorkDir(), path)
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return llm.NewTextResult(fmt.Sprintf("Error: %v", err)), nil
		}

		var names []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		sort.Strings(names)

		content := TruncateToolOutput(a, fmt.Sprintf("<path>%s</path>\n<entries>\n%s\n</entries>",
			path, strings.Join(names, "\n")))
		return llm.NewTextResult(content), nil
	})

	return &ToolDef{
		Name:        "ls",
		Description: desc,
		Template:    "ls.tool.tmpl",
		Handler:     h,
	}
}
