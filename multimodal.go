package toroid

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/fantasy"
	"github.com/yashbonde/toroid-kernel/tools"
)

// imageRefRe matches markdown image syntax — ![alt](path) — used to inline
// local images into a user turn.
var imageRefRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

// parseUserMessage turns a raw prompt into a user Message and the string that
// should be persisted for it. "text ![x](path) text" is loaded as text→image→text,
// which fantasy.NewUserMessage cannot express. x -> ~/.../path absolute path, so a
// session resumed from any directory still resolves to the same file. Refs that
// don't resolve are left as literal text — the model simply sees a broken link.
func parseUserMessage(prompt, workDir string) (fantasy.Message, string) {
	var parts []fantasy.MessagePart
	var stored, text strings.Builder
	flush := func() { // emit accumulated text (if any) as one part
		if t := strings.TrimSpace(text.String()); t != "" {
			parts = append(parts, fantasy.TextPart{Text: t})
		}
		text.Reset()
	}

	last := 0
	for _, m := range imageRefRe.FindAllStringSubmatchIndex(prompt, -1) {
		before := prompt[last:m[0]]
		stored.WriteString(before)
		text.WriteString(before)

		if fp, tilde, ok := loadFilePart(prompt[m[2]:m[3]], workDir); ok {
			flush() // image breaks the run of text
			parts = append(parts, fp)
			stored.WriteString(prompt[m[0]:m[2]]) // "![alt]("
			stored.WriteString(tilde)             // ~-rooted absolute path
			stored.WriteString(prompt[m[3]:m[1]]) // ")"
		} else {
			lit := prompt[m[0]:m[1]] // unresolved ref stays inline as text
			stored.WriteString(lit)
			text.WriteString(lit)
		}
		last = m[1]
	}
	tail := prompt[last:]
	stored.WriteString(tail)
	text.WriteString(tail)
	flush()

	if len(parts) == 0 { // whitespace-only or empty prompt: preserve verbatim
		parts = append(parts, fantasy.TextPart{Text: prompt})
	}
	return fantasy.Message{Role: fantasy.MessageRoleUser, Content: parts}, stored.String()
}

// loadFilePart resolves an inline image ref and, if it points at a readable
// model-supported media file, returns the file part plus its ~-rooted absolute
// path for persistence. ok is false for unsupported types or unreadable files.
func loadFilePart(ref, workDir string) (part fantasy.FilePart, tildePath string, ok bool) {
	abs := tools.ResolvePath(ref, workDir)
	mt, ok := tools.MediaType(abs)
	if !ok {
		return fantasy.FilePart{}, "", false
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fantasy.FilePart{}, "", false
	}
	return fantasy.FilePart{Filename: filepath.Base(abs), Data: data, MediaType: mt}, pathToTilde(abs), true
}

// pathToTilde rewrites an absolute path under the user's home as "~/…" so the
// persisted reference is independent of the directory the session runs from.
// Paths outside home are returned unchanged (still absolute, still portable).
func pathToTilde(abs string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel := strings.TrimPrefix(abs, home+string(filepath.Separator)); rel != abs {
			return "~/" + rel
		}
	}
	return abs
}
