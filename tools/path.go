package tools

import (
	"os"
	"path/filepath"
	"strings"
)

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
