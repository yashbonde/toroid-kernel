package main

import (
	"fmt"
	"regexp"
	"strings"
)

// A tiny, dependency-free ANSI renderer for the subset of Markdown that LLM
// answers actually use: headings, fenced/inline code, bold/italic, bullet and
// numbered lists, blockquotes, and horizontal rules. It is deliberately small —
// the goal is a readable terminal, not a spec-complete CommonMark engine.

// SGR escape sequences. Vars (not consts) so disableColor can blank them all
// when NO_COLOR is set; named so the rendering rules read as intent.
var (
	aReset     = "\x1b[0m"
	aBold      = "\x1b[1m"
	aDim       = "\x1b[2m"
	aItalic    = "\x1b[3m"
	aUnderline = "\x1b[4m"
	aStrikethrough = "\x1b[9m"

	aRed     = "\x1b[31m"
	aGreen   = "\x1b[32m"
	aYellow  = "\x1b[33m"
	aBlue    = "\x1b[34m"
	aMagenta = "\x1b[35m"
	aCyan    = "\x1b[36m"
	aGray    = "\x1b[90m"

	aHeading = aBold + aCyan
)

// disableColor blanks every styling code so all rendering becomes plain text.
func disableColor() {
	aReset, aBold, aDim, aItalic, aUnderline, aStrikethrough = "", "", "", "", "", ""
	aRed, aGreen, aYellow, aBlue, aMagenta, aCyan, aGray = "", "", "", "", "", "", ""
	aHeading = ""
}

var (
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reBoldStar   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reBoldUnder  = regexp.MustCompile(`__([^_]+)__`)
	reStrike     = regexp.MustCompile(`~~([^~]+)~~`)
	reItalicStar = regexp.MustCompile(`\*([^*]+)\*`)
	reLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reBullet     = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reNumbered   = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	reHeading    = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
)

// renderMarkdown turns a markdown string into ANSI-decorated text wrapped to
// width. Fenced code blocks are passed through verbatim (only colored), never
// reflowed, so code stays valid.
func renderMarkdown(src string, width int) string {
	if width < 20 {
		width = 80
	}
	var out strings.Builder
	lines := strings.Split(strings.TrimRight(src, "\n"), "\n")

	inCode := false
	codeLang := ""
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// Fenced code block toggle.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inCode {
				inCode = true
				codeLang = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "```"))
				label := codeLang
				if label == "" {
					label = "code"
				}
				out.WriteString(aGray + "┌─ " + label + " " + aReset + "\n")
			} else {
				inCode = false
				out.WriteString(aGray + "└─" + aReset + "\n")
			}
			continue
		}
		if inCode {
			out.WriteString(aYellow + line + aReset + "\n")
			continue
		}

		// Horizontal rule.
		if t := strings.TrimSpace(line); t == "---" || t == "***" || t == "___" {
			out.WriteString(aGray + strings.Repeat("─", width) + aReset + "\n")
			continue
		}

		// Headings.
		if m := reHeading.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			prefix := strings.Repeat("#", level) + " "
			out.WriteString(aHeading + prefix + inline(m[2]) + aReset + "\n")
			continue
		}

		// Blockquote.
		if strings.HasPrefix(line, ">") {
			body := strings.TrimSpace(strings.TrimPrefix(line, ">"))
			out.WriteString(aGray + "▏ " + aItalic + inline(body) + aReset + "\n")
			continue
		}

		// Bullet list.
		if m := reBullet.FindStringSubmatch(line); m != nil {
			indent := m[1]
			out.WriteString(indent + aCyan + "• " + aReset + wrapHang(inline(m[2]), width, len(indent)+2) + "\n")
			continue
		}
		// Numbered list.
		if m := reNumbered.FindStringSubmatch(line); m != nil {
			indent := m[1]
			marker := m[2] + ". "
			out.WriteString(indent + aCyan + marker + aReset + wrapHang(inline(m[3]), width, len(indent)+len(marker)) + "\n")
			continue
		}

		// Blank line passes through.
		if strings.TrimSpace(line) == "" {
			out.WriteString("\n")
			continue
		}

		// Plain paragraph: inline-format, then wrap.
		out.WriteString(wrapHang(inline(line), width, 0) + "\n")
	}
	return out.String()
}

// inline applies inline markdown (code, bold, italic, links) using ANSI codes.
// Inline code is pulled out to placeholders FIRST so its contents are neither
// re-interpreted as bold/italic nor confused for link brackets by the ANSI
// escape codes we inject (which contain '[') — then spliced back styled at the end.
func inline(s string) string {
	var codes []string
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		body := strings.Trim(m, "`")
		codes = append(codes, aBlue+" "+body+" "+aReset)
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	})

	s = reLink.ReplaceAllString(s, aUnderline+aBlue+"$1"+aReset+aGray+" ($2)"+aReset)
	s = reStrike.ReplaceAllString(s, aStrikethrough+"$1"+aReset)
	s = reBoldStar.ReplaceAllString(s, aBold+"$1"+aReset)
	s = reBoldUnder.ReplaceAllString(s, aBold+"$1"+aReset)
	s = reItalicStar.ReplaceAllString(s, aItalic+"$1"+aReset)

	for i, c := range codes {
		s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), c, 1)
	}
	return s
}

// visibleLen counts printable runes, skipping ANSI escape sequences, so wrapping
// math isn't thrown off by invisible control codes.
func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// still inside escape
		default:
			n++
		}
	}
	return n
}

// wrapHang word-wraps ANSI-decorated text to width, indenting continuation lines
// by hang spaces so list/quote bodies stay aligned under their marker.
func wrapHang(s string, width, hang int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	pad := strings.Repeat(" ", hang)
	var b strings.Builder
	lineLen := 0
	for i, w := range words {
		wl := visibleLen(w)
		if i == 0 {
			b.WriteString(w)
			lineLen = wl
			continue
		}
		if lineLen+1+wl > width {
			b.WriteString("\n" + pad + w)
			lineLen = hang + wl
		} else {
			b.WriteString(" " + w)
			lineLen += 1 + wl
		}
	}
	return b.String()
}

// trimOneLine collapses s to a single line and truncates it to max visible
// characters, appending an ellipsis with the original length when cut. Used to
// keep tool args/results from eating the whole screen.
func trimOneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		nlines := strings.Count(s, "\n") + 1
		s = strings.TrimSpace(s[:idx])
		s = fmt.Sprintf("%s %s(+%d lines)%s", s, aDim, nlines-1, aReset)
	}
	runes := []rune(s)
	// Recompute on the visible slice; escape codes here are minimal.
	if len(runes) > max {
		return string(runes[:max]) + aDim + "…" + aReset
	}
	return s
}
