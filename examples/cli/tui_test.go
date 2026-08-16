package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	toroid "github.com/yashbonde/toroid-kernel"
)

func TestPaintInputSurfaceFillsEveryRow(t *testing.T) {
	got := paintInputSurface("hello", 24, 3)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d rows, want 3", len(lines))
	}
	for i, line := range lines {
		if width := ansi.StringWidth(line); width != 24 {
			t.Errorf("row %d width = %d, want 24", i, width)
		}
	}
}

func TestToolOutputJoinsAssistantWithoutIndent(t *testing.T) {
	m := &tuiModel{width: 80}
	m.appendActivity("  bash  go test ./...\n  passed\n")
	if len(m.entries) != 1 || m.entries[0].kind != transcriptAssistant {
		t.Fatalf("tool output entry = %#v", m.entries)
	}
	if strings.HasPrefix(m.entries[0].text, " ") || strings.Contains(m.entries[0].text, "\n ") {
		t.Fatalf("tool output retained indentation: %q", m.entries[0].text)
	}
}

func TestToolResultSummaryDoesNotExposeArbitraryPreview(t *testing.T) {
	result := "/very/long/path/that/should/not/become/the/tool/label\nline two\nline three"
	got := toolResultSummary(result)
	if strings.Contains(got, "/very/long/path") {
		t.Fatalf("summary leaked result preview: %q", got)
	}
	if got != "done · 3 lines · 73 bytes" {
		t.Fatalf("summary = %q", got)
	}
}

func TestResizeReservesComposerAndUsesVisualInputHeight(t *testing.T) {
	input := textarea.New()
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.DynamicHeight = true
	input.MinHeight = 3
	input.MaxContentHeight = 10000
	input.SetValue(strings.Repeat("wrapped input ", 30))
	transcript := viewport.New()
	transcript.FillHeight = true
	m := &tuiModel{width: 48, height: 24, input: input, transcript: transcript, cfg: &config{model: "test"}, kernel: &toroid.Kernel{Cfg: toroid.Config{TotalContextSize: 200000}}}
	m.entries = []transcriptEntry{{kind: transcriptAssistant, text: strings.Repeat("long response\n", 100)}}

	for _, size := range []struct{ width, height int }{{80, 30}, {48, 24}, {36, 18}} {
		m.width, m.height = size.width, size.height
		m.resize()
		if m.input.Height() <= 3 {
			t.Fatalf("wrapped input did not grow composer at %dx%d: height=%d", size.width, size.height, m.input.Height())
		}
		if got := m.transcript.Height() + m.input.Height() + 7; got > m.height {
			t.Fatalf("layout uses %d rows in a %d-row terminal", got, m.height)
		}
		if got := lipgloss.Height(m.View().Content); got > m.height {
			t.Fatalf("rendered view is %d rows in a %d-row terminal (viewport=%d/%d input=%d/%d)", got, m.height, lipgloss.Height(m.transcript.View()), m.transcript.Height(), lipgloss.Height(m.input.View()), m.input.Height())
		}
		for row, line := range strings.Split(m.View().Content, "\n") {
			if got := ansi.StringWidth(line); got > m.width {
				t.Fatalf("rendered row %d is %d columns in a %d-column terminal", row, got, m.width)
			}
		}
		lines := strings.Split(m.View().Content, "\n")
		if last := strings.TrimSpace(ansi.Strip(lines[len(lines)-1])); !strings.HasPrefix(last, "$0.00") {
			t.Fatalf("final terminal row does not contain status bar: %q", last)
		}
	}
}

func TestResizePreservesViewportScrollPosition(t *testing.T) {
	input := textarea.New()
	input.DynamicHeight = true
	input.MinHeight = 3
	input.MaxContentHeight = 10000
	transcript := viewport.New()
	m := &tuiModel{width: 80, height: 24, input: input, transcript: transcript, entries: []transcriptEntry{{kind: transcriptAssistant, text: strings.Repeat("A complete paragraph that occupies its own rendered row.\n\n", 100)}}}
	m.resize()
	m.transcript.SetYOffset(10)

	m.resize()
	if got := m.transcript.YOffset(); got != 10 {
		t.Fatalf("resize reset scroll position to %d", got)
	}
}

func TestTranscriptHasNoTrailingOverflowRow(t *testing.T) {
	transcript := viewport.New()
	transcript.SoftWrap = true
	transcript.SetWidth(20)
	m := &tuiModel{
		width:      20,
		transcript: transcript,
		entries:    []transcriptEntry{{kind: transcriptAssistant, text: "12345678901234567890", done: true}},
	}
	m.renderTranscript()
	// Leading gap (1) + model header (1) + blank separator (1) + content (1)
	// + timestamp (1) + trailing gap newline (1).
	if got := m.transcript.TotalLineCount(); got != 6 {
		t.Fatalf("exact-width assistant entry rendered as %d rows, want 6", got)
	}
}

func TestNormalizeTranscriptSpacingCollapsesBlankRuns(t *testing.T) {
	input := "first\n\n\n\nsecond\n  \n\t\nthird\n\n"
	want := "first\n\nsecond\n\nthird"
	if got := normalizeTranscriptSpacing(input); got != want {
		t.Fatalf("normalized spacing = %q, want %q", got, want)
	}
}

func TestAssistantRendererIsWidthBoundedAndUnclipped(t *testing.T) {
	got := renderAssistantText("one two three four five\n\n\nlast", 10)
	if !strings.HasSuffix(strings.TrimSpace(ansi.Strip(got)), "last") {
		t.Fatalf("renderer clipped final content: %q", got)
	}
	for i, line := range strings.Split(got, "\n") {
		if width := ansi.StringWidth(line); width > 10 {
			t.Fatalf("line %d width = %d, want <= 10: %q", i, width, line)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("renderer retained excessive blank lines: %q", got)
	}
}

func TestAssistantRendererRendersMarkdown(t *testing.T) {
	got := renderAssistantText("## Result\n\nUse **bold** and `code`.\n\n| Name | Value |\n|---|---|\n| files | 11 |", 60)
	plain := ansi.Strip(got)
	for _, markdownToken := range []string{"## Result", "**bold**", "`code`", "|---|"} {
		if strings.Contains(plain, markdownToken) {
			t.Fatalf("markdown token %q was not rendered:\n%s", markdownToken, plain)
		}
	}
	for _, content := range []string{"Result", "bold", "code", "files", "11"} {
		if !strings.Contains(plain, content) {
			t.Fatalf("rendered markdown lost %q:\n%s", content, plain)
		}
	}
	for row, line := range strings.Split(got, "\n") {
		if width := ansi.StringWidth(line); width > 60 {
			t.Fatalf("markdown row %d is %d columns, want <= 60: %q", row, width, line)
		}
	}
}

func TestViewLeavesMouseAvailableForTerminalSelection(t *testing.T) {
	input := textarea.New()
	input.DynamicHeight = true
	input.MinHeight = 3
	input.MaxContentHeight = 10000
	transcript := viewport.New()
	m := &tuiModel{
		width: 80, height: 24, input: input, transcript: transcript,
		cfg: &config{model: "test"}, kernel: &toroid.Kernel{Cfg: toroid.Config{TotalContextSize: 200000}},
	}
	m.resize()
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("mouse mode = %v; terminal selection requires MouseModeNone", got)
	}
}
