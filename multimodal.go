package toroid

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// maxInlineMediaBytes is the hard cap on a single inlined image/PDF part (M8).
// Oversized media is dropped (kept as literal text) with a warning rather than
// silently embedded — an unbounded base64 blob bloats every subsequent request
// and can blow the context window. 5 MiB comfortably covers screenshots and
// document pages while bounding worst-case request size.
const maxInlineMediaBytes = 5 << 20 // 5 MiB

// imageRefRe matches markdown image syntax — ![alt](path) — used to inline
// local images into a user turn.
var imageRefRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

// parseUserMessage turns a raw prompt into a user Message, the string that
// should be persisted for it, and any human-readable warnings about media that
// could not be inlined. "text ![x](path) text" is loaded as text→image→text.
// x -> ~/.../path absolute path, so a session resumed from any directory still
// resolves to the same file.
//
// A media ref is inlined only when it resolves to a readable, supported file
// within the size cap AND the model accepts image input; otherwise the ref is
// left as literal text (the model sees a link) and a warning is returned so the
// caller can signal the drop instead of failing silently (M8).
func parseUserMessage(prompt, workDir string, model Model) (llm.Message, string, []string) {
	var parts []llm.Part
	var stored, text strings.Builder
	var warnings []string
	flush := func() { // emit accumulated text (if any) as one part
		if t := strings.TrimSpace(text.String()); t != "" {
			parts = append(parts, llm.TextPart{Text: t})
		}
		text.Reset()
	}

	last := 0
	for _, m := range imageRefRe.FindAllStringSubmatchIndex(prompt, -1) {
		before := prompt[last:m[0]]
		stored.WriteString(before)
		text.WriteString(before)

		ref := prompt[m[2]:m[3]]
		if fp, tilde, ok, warn := loadFilePart(ref, workDir, model); ok {
			flush() // image breaks the run of text
			parts = append(parts, fp)
			stored.WriteString(prompt[m[0]:m[2]]) // "![alt]("
			stored.WriteString(tilde)             // ~-rooted absolute path
			stored.WriteString(prompt[m[3]:m[1]]) // ")"
		} else {
			if warn != "" {
				warnings = append(warnings, warn)
			}
			lit := prompt[m[0]:m[1]] // unresolved/rejected ref stays inline as text
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
		parts = append(parts, llm.TextPart{Text: prompt})
	}
	return llm.Message{Role: llm.RoleUser, Parts: parts}, stored.String(), warnings
}

// loadFilePart resolves an inline image ref and, if it points at a readable,
// model-supported media file within the size cap, returns the file part plus its
// ~-rooted absolute path for persistence. ok is false when the ref cannot be
// inlined; warn is a non-empty explanation when the drop is worth surfacing
// (unsupported model, oversized file). A ref that simply isn't a media file
// (ordinary markdown link) returns ok=false with an empty warn.
func loadFilePart(ref, workDir string, model Model) (part llm.FilePart, tildePath string, ok bool, warn string) {
	abs := tools.ResolvePath(ref, workDir)
	mt, isMedia := tools.MediaType(abs)
	if !isMedia {
		return llm.FilePart{}, "", false, "" // not a media ref; leave as text
	}
	// The ref points at real media — from here, any failure is worth a warning
	// so an intended image is never silently discarded.
	if !model.SupportsImage() {
		return llm.FilePart{}, "", false,
			fmt.Sprintf("model %q does not accept image input; %s left as text (not sent as an image)", model.ID, ref)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return llm.FilePart{}, "", false,
			fmt.Sprintf("could not read media %s: %v", ref, err)
	}
	if info.Size() > maxInlineMediaBytes {
		return llm.FilePart{}, "", false,
			fmt.Sprintf("media %s is %d bytes, over the %d-byte inline cap; left as text", ref, info.Size(), maxInlineMediaBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return llm.FilePart{}, "", false,
			fmt.Sprintf("could not read media %s: %v", ref, err)
	}
	// Persist the path ~-rooted so a session resumed from any directory still
	// resolves the same file; paths outside home stay absolute.
	tilde := abs
	if home, err := os.UserHomeDir(); err == nil {
		if rel := strings.TrimPrefix(abs, home+string(filepath.Separator)); rel != abs {
			tilde = "~/" + rel
		}
	}
	return llm.FilePart{Filename: filepath.Base(abs), Data: data, MediaType: mt}, tilde, true, ""
}
