package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/yashbonde/toroid-kernel/tools"
)

// Files: the agent's second tool group. Documents the user asks to keep are
// saved in shared storage at Documents/Toroid, so they show in the phone's
// Files app (Documents › Toroid). As with the to-dos, the Go server does no
// file I/O itself: the tools validate their arguments here and forward them to
// the tool host (ToolHost.java, Files.java), which writes the files, tells the
// media scanner, and opens them in a viewer. The host checks every name again
// and keeps every path inside that one directory.

const (
	filesDir     = "Documents/Toroid" // where the files are, as the Files app shows it
	maxSaveBytes = 1 << 20            // bytes in one saved file
	maxNameLen   = 80                 // characters in a file name before its extension
)

// fileNamePattern is every name the file tools accept: one file name, no
// path, no leading dot (hidden) or space, and an extension.
var fileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_()-][A-Za-z0-9 ._()-]{0,79}\.[A-Za-z0-9]{1,10}$`)

// textFormats are the extensions file_save writes. Everything it saves is
// text; media files are only listed, opened and deleted.
var textFormats = map[string]bool{
	"html": true, "htm": true, "txt": true, "md": true, "markdown": true,
	"json": true, "csv": true, "tsv": true, "xml": true, "yaml": true, "yml": true,
	"toml": true, "ini": true, "log": true, "css": true, "js": true, "ts": true,
	"py": true, "go": true, "java": true, "kt": true, "c": true, "h": true,
	"cpp": true, "rs": true, "rb": true, "swift": true, "sh": true, "sql": true,
}

// formatAliases are words a model may pass as format instead of an extension.
var formatAliases = map[string]string{
	"text": "txt", "plain": "txt", "plaintext": "txt", "markdown": "md", "document": "html", "doc": "html",
}

func isFileOp(op string) bool { return strings.HasPrefix(op, "file") }

// checkFileName validates one file name and returns it with its whitespace
// tidied.
func checkFileName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	switch {
	case name == "":
		return "", errors.New("name is empty")
	case strings.ContainsAny(name, `/\`):
		return "", fmt.Errorf("bad name %q: give a file name only, no folders", name)
	case strings.Contains(name, ".."):
		return "", fmt.Errorf("bad name %q: no \"..\" in names", name)
	case strings.HasPrefix(name, "."):
		return "", fmt.Errorf("bad name %q: names cannot start with a dot", name)
	}
	ext := extOf(name)
	if ext == "" {
		return "", fmt.Errorf("bad name %q: it needs an extension, such as .html or .txt", name)
	}
	if n := len(name) - len(ext) - 1; n > maxNameLen {
		return "", fmt.Errorf("bad name %q: keep it to %d characters before the extension", oneLine(name, 40), maxNameLen)
	}
	if !fileNamePattern.MatchString(name) {
		return "", fmt.Errorf("bad name %q: use letters, digits, spaces and . _ ( ) - only", name)
	}
	return name, nil
}

// extOf is name's extension, lower-cased, without the dot ("" if none).
func extOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// saveName decides the name a file is saved under. A name that already ends
// in a text format's extension keeps it; otherwise the format's extension is
// added (so "Notes v1.2" becomes "Notes v1.2.html"). With no format, a
// document is HTML.
func saveName(name, format string) (string, error) {
	name = strings.TrimRight(strings.Join(strings.Fields(name), " "), ". ")
	format = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
	if a, ok := formatAliases[format]; ok {
		format = a
	}
	if format != "" && !textFormats[format] {
		return "", fmt.Errorf("cannot save as %q: file_save writes text formats only (html, txt, md, json, csv and the like)", format)
	}
	if ext := extOf(name); textFormats[ext] {
		if format != "" && !sameFormat(ext, format) {
			return "", fmt.Errorf("name %q ends in .%s but format is %s: make them agree", name, ext, format)
		}
		return checkFileName(name)
	} else if ext != "" && isMediaOrBinaryExt(ext) {
		return "", fmt.Errorf("cannot save %q: file_save writes text, and .%s is not a text format", name, ext)
	}
	if format == "" {
		format = "html"
	}
	if name == "" {
		return "", errors.New("name is empty")
	}
	return checkFileName(name + "." + format)
}

func sameFormat(ext, format string) bool {
	norm := func(s string) string {
		switch s {
		case "htm":
			return "html"
		case "markdown":
			return "md"
		case "yml":
			return "yaml"
		}
		return s
	}
	return norm(ext) == norm(format)
}

// isMediaOrBinaryExt reports the common extensions that are not text, so that
// "photo.jpg" is refused rather than saved as "photo.jpg.html".
func isMediaOrBinaryExt(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png", "gif", "webp", "heic", "bmp", "svg",
		"mp4", "mkv", "webm", "3gp", "mov", "avi",
		"mp3", "m4a", "aac", "ogg", "opus", "wav", "flac",
		"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "zip", "apk":
		return true
	}
	return false
}

// Tool arguments.

type filesListArgs struct{}

type fileSaveArgs struct {
	Name      string `json:"name" jsonschema:"description=File name without folders (letters digits spaces . _ ( ) -). With no extension the format's is added,minLength=1,maxLength=92"`
	Content   string `json:"content" jsonschema:"description=The whole file. For HTML a complete self-contained document,minLength=1"`
	Format    string `json:"format,omitempty" jsonschema:"description=html (the default for a document) or the format the user named: txt or md or json or csv and so on"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"description=Replace an existing file of that name. Only when the user asked to replace it"`
}

type fileNameArgs struct {
	Name string `json:"name" jsonschema:"description=The file's name as files_list shows it (e.g. Packing list.html),minLength=1,maxLength=92"`
}

func (a *filesListArgs) validate() error { return nil }

func (a *fileSaveArgs) validate() (err error) {
	if a.Name, err = saveName(a.Name, a.Format); err != nil {
		return err
	}
	a.Format = "" // the name now carries it
	if strings.TrimSpace(a.Content) == "" {
		return errors.New("content is empty")
	}
	if len(a.Content) > maxSaveBytes {
		return fmt.Errorf("content is %d bytes; a saved file can be at most 1 MB", len(a.Content))
	}
	return nil
}

func (a *fileNameArgs) validate() (err error) {
	a.Name, err = checkFileName(a.Name)
	return err
}

// registerFileTools adds the five file tools to r.
func registerFileTools(r *tools.Registry, h *toolHost) {
	r.Register(hostTool[filesListArgs](h, "files_list",
		"List the files in the user's Documents/Toroid folder (name, size in bytes, modified time, MIME type), newest first."))
	r.Register(hostTool[fileSaveArgs](h, "file_save",
		"Save a text file in Documents/Toroid, where the user sees it in the Files app. Only when the user explicitly asks to store or save something. "+
			"With no format named, save a self-contained HTML document. Refuses to replace an existing file unless overwrite is true. Returns name, path and bytes."))
	r.Register(hostTool[fileNameArgs](h, "file_read",
		"Read a text file from Documents/Toroid (the first 64 KB). Media files cannot be read; open them instead."))
	r.Register(hostTool[fileNameArgs](h, "file_open",
		"Show a file from Documents/Toroid on the phone's screen: HTML as a web page, images and video in Gallery, audio in Music, other text as plain text. "+
			"Only when the user asks to see or open it."))
	r.Register(hostTool[fileNameArgs](h, "file_delete",
		"Delete a file from Documents/Toroid for good. Only when the user explicitly asks."))
}

// fileStartText is the line the UI shows while a file tool runs.
func fileStartText(name string) string {
	switch name {
	case "files_list":
		return "… listing files"
	case "file_save":
		return "… saving a file"
	case "file_read":
		return "… reading a file"
	case "file_open":
		return "… opening a file"
	case "file_delete":
		return "… deleting a file"
	}
	return "… " + name
}

// fileResult is the union of the host's file results.
type fileResult struct {
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	OpenedAs  string `json:"opened_as"`
	Truncated bool   `json:"truncated"`
}

// fileSummary is the one line for a finished file tool call: "✓ saved:
// Packing list.html (2 KB) · Documents/Toroid", "✓ opened: notes.md as text",
// "✓ listed 3 files", "✓ deleted: old.txt".
func fileSummary(name, result string) string {
	if name == "files_list" {
		var list []json.RawMessage
		if json.Unmarshal([]byte(result), &list) != nil {
			return "✓ listed files"
		}
		if len(list) == 1 {
			return "✓ listed 1 file"
		}
		return fmt.Sprintf("✓ listed %d files", len(list))
	}
	var f fileResult
	_ = json.Unmarshal([]byte(result), &f)
	verb := map[string]string{"file_save": "saved", "file_read": "read", "file_open": "opened", "file_delete": "deleted"}[name]
	if verb == "" {
		return "✓ " + name
	}
	if f.Name == "" {
		return "✓ " + verb
	}
	line := "✓ " + verb + ": " + oneLine(f.Name, 80)
	switch name {
	case "file_save":
		line += " (" + humanSize(f.Bytes) + ") · " + filesDir
	case "file_read":
		if f.Truncated {
			line += " (first " + humanSize(f.Bytes) + ")"
		} else {
			line += " (" + humanSize(f.Bytes) + ")"
		}
	case "file_open":
		if f.OpenedAs != "" {
			line += " as " + f.OpenedAs
		}
	}
	return line
}

// humanSize is "512 B", "2 KB" or "1.4 MB".
func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", (n+512)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}
