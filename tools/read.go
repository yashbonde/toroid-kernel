package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"charm.land/fantasy"
)

const (
	DefaultReadLimit = 2000
	MaxLineLength    = 2000
)

type ReadArgs struct {
	FilePath string `json:"filePath" jsonschema:"description=The absolute path to the file or directory to read"`
	Offset   int    `json:"offset,omitempty" jsonschema:"description=The line number to start reading from (1-indexed),default=1"`
	Limit    int    `json:"limit,omitempty" jsonschema:"description=The maximum number of lines to read (defaults to 2000),default=2000"`
}

func (a ReadArgs) Validate() error {
	if a.FilePath == "" {
		return fmt.Errorf("filePath is required")
	}
	if a.Offset < 0 {
		return fmt.Errorf("offset must be >= 0")
	}
	return nil
}

func NewReadTool(a Agent, desc string) *ToolDef {
	// errf builds an error tool response; toroid keys tool failure off the
	// "Error:" content prefix, so every failure path funnels through here.
	errf := func(format string, a ...any) fantasy.ToolResponse {
		return fantasy.NewTextErrorResponse("Error: " + fmt.Sprintf(format, a...))
	}

	fTool := fantasy.NewAgentTool("read", desc, func(ctx context.Context, args ReadArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if err := args.Validate(); err != nil {
			return errf("%v", err), nil
		}

		path := ResolvePath(args.FilePath, a.WorkDir())

		offset := args.Offset
		if offset <= 0 {
			offset = 1
		}
		limit := args.Limit
		if limit == 0 {
			limit = DefaultReadLimit
		}

		info, err := os.Stat(path)
		if err != nil {
			return errf("%v", err), nil
		}

		var content string
		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return errf("%v", err), nil
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

			start := offset - 1
			if start >= len(names) {
				content = fmt.Sprintf("<path>%s</path>\n<type>directory</type>\n<entries>\n(No more entries)\n</entries>", path)
			} else {
				end := start + limit
				if end > len(names) {
					end = len(names)
				}
				sliced := names[start:end]
				content = fmt.Sprintf("<path>%s</path>\n<type>directory</type>\n<entries>\n%s\n(%d entries)\n</entries>",
					path, strings.Join(sliced, "\n"), len(names))
			}
		} else {
			// File reading
			f, err := os.Open(path)
			if err != nil {
				return errf("%v", err), nil
			}
			defer f.Close()

			// Simple binary check
			isBinary, err := isBinaryFile(f)
			if err != nil {
				return errf("%v", err), nil
			}
			if isBinary {
				// Images and PDFs are returned as media attachments so a
				// vision-capable model can see them; other binaries are refused.
				if mt, ok := MediaType(path); ok {
					data, err := os.ReadFile(path)
					if err != nil {
						return errf("%v", err), nil
					}
					resp := fantasy.NewMediaResponse(data, mt)
					resp.Content = fmt.Sprintf("Read %s (%d bytes, %s)", path, len(data), mt)
					return resp, nil
				}
				return errf("cannot read binary file: %s", path), nil
			}
			f.Seek(0, 0)

			scanner := bufio.NewScanner(f)
			var raw []string
			lineNum := 0
			for scanner.Scan() {
				lineNum++
				if lineNum < offset {
					continue
				}
				if len(raw) >= limit {
					break
				}
				text := scanner.Text()
				if len(text) > MaxLineLength {
					text = text[:MaxLineLength] + "... (line truncated)"
				}
				raw = append(raw, fmt.Sprintf("%d: %s", lineNum, text))
			}

			content = fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s\n", path, strings.Join(raw, "\n"))
			if scanner.Scan() || lineNum >= offset+limit {
				lastLine := offset + len(raw) - 1
				if lastLine < offset {
					lastLine = offset
				}
				content += fmt.Sprintf("\n(Showing lines %d-%d. Use offset=%d to continue.)", offset, lastLine, offset+len(raw))
			} else {
				content += fmt.Sprintf("\n(End of file - total %d lines)", lineNum)
			}
			content += "\n</content>"
		}

		return fantasy.NewTextResponse(content), nil
	})

	return &ToolDef{
		Name:        "read",
		Description: desc,
		Template:    "read.tool.tmpl",
		AgentTool:   fTool,
	}
}

func isBinaryFile(f *os.File) (bool, error) {
	buf := make([]byte, 1024)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return false, nil
	}
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true, nil
		}
	}
	return false, nil
}
