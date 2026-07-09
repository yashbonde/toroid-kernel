package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxToolOutputChars is the shared hard cap for tool result text returned to
// the model. Unbounded grep/MCP dumps otherwise re-enter the next prompt at
// full size. ~20k chars ≈ 5k tokens; bash already used this budget.
const MaxToolOutputChars = 20_000

// TruncateToolOutput clips s to MaxToolOutputChars and appends a short note
// when truncated so the model knows more content was omitted.
func TruncateToolOutput(s string) string {
	if len(s) <= MaxToolOutputChars {
		return s
	}
	omitted := len(s) - MaxToolOutputChars
	return s[:MaxToolOutputChars] + fmt.Sprintf("\n… [truncated %d bytes]", omitted)
}

// ResolvePath expands a leading "~/" to the user's home directory and makes
// relative paths absolute against workDir. The result is always cleaned. It is
// the single path-resolution helper shared by the read tool and the kernel's
// inline-image parser.
func ResolvePath(p, workDir string) string {
	switch {
	case strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	case !filepath.IsAbs(p):
		p = filepath.Join(workDir, p)
	}
	return filepath.Clean(p)
}

// MediaType returns the IANA media type for a path when it is a model-supported
// image or PDF, with ok=false for anything else. Both the read tool (to decide
// whether a binary file can be returned as a media attachment) and the kernel's
// inline-image parser (to decide whether an "![](…)" ref becomes a file part)
// gate on this.
func MediaType(path string) (mediaType string, ok bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	case ".pdf":
		return "application/pdf", true
	}
	return "", false
}
