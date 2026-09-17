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
		if last := strings.TrimSpace(ansi.Strip(lines[len(lines)-1])); !strings.HasPrefix(last, "₹0.00") {
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
	// Content (1) + blank separator (1) + timestamp (1).
	if got := m.transcript.TotalLineCount(); got != 3 {
		t.Fatalf("exact-width assistant entry rendered as %d rows, want 3", got)
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

func TestToolCallLabelIsSingleLineWithoutBoxArt(t *testing.T) {
	got := toolCallLabel("bash", `{"command":"git ls-files | wc -l"}`, "/work")
	if !strings.HasPrefix(got, "bash  ") {
		t.Fatalf("label = %q, want it to start with the tool name", got)
	}
	if strings.Contains(got, "┌") || strings.Contains(got, "└") {
		t.Fatalf("label retained box art: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("label is not a single line: %q", got)
	}
}

func TestClipToolLabelCapsAtFiftyChars(t *testing.T) {
	long := strings.Repeat("x", 120)
	got := clipToolLabel(long)
	if len([]rune(got)) != maxToolLabelWidth {
		t.Fatalf("clipped label length = %d, want %d: %q", len([]rune(got)), maxToolLabelWidth, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("clipped label missing ellipsis: %q", got)
	}
	short := "bash  git status"
	if got := clipToolLabel(short); got != short {
		t.Fatalf("short label should be unchanged, got %q", got)
	}
	exact := strings.Repeat("y", maxToolLabelWidth)
	if got := clipToolLabel(exact); got != exact {
		t.Fatalf("exactly-%d-char label should be unchanged, got %q", maxToolLabelWidth, got)
	}
}

func TestToolLifecycleShowsLiveAndCommittedRows(t *testing.T) {
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
	m.busy = true

	m.startTool("id1", "read  foo.go", 1)
	m.startTool("id2", "bash  ls", 2)
	m.spinnerFrame = 0
	live := ansi.Strip(m.transcript.View())
	// Both concurrent tools should show a live working row at once.
	if !strings.Contains(live, "read  foo.go") {
		t.Fatalf("live tool row missing first call text:\n%q", live)
	}
	if !strings.Contains(live, "bash  ls") {
		t.Fatalf("live tool row missing second call text:\n%q", live)
	}
	if len(m.runningTools) != 2 {
		t.Fatalf("runningTools = %d, want 2", len(m.runningTools))
	}

	// Finishing one call removes only that row and commits it in bold (no
	// "done!" suffix); the other stays live.
	m.finishTool("id1", "")
	committed := ansi.Strip(m.transcript.View())
	if strings.Contains(committed, "done!") {
		t.Fatalf("committed tool row should have no 'done!' suffix:\n%q", committed)
	}
	if !strings.Contains(committed, "read  foo.go") {
		t.Fatalf("committed tool row missing call text:\n%q", committed)
	}
	if len(m.runningTools) != 1 || m.runningTools[0].id != "id2" {
		t.Fatalf("runningTools after first finish = %#v, want only id2", m.runningTools)
	}
	if !strings.Contains(ansi.Strip(m.transcript.View()), "bash  ls") {
		t.Fatalf("second live tool row vanished after first finish:\n%q", committed)
	}

	// A failed call commits with its error text.
	m.finishTool("id2", "boom")
	errored := ansi.Strip(m.transcript.View())
	if !strings.Contains(errored, "bash  ls ⨯ boom") {
		t.Fatalf("failed tool row missing error text:\n%q", errored)
	}
	if len(m.runningTools) != 0 {
		t.Fatalf("runningTools not cleared after all finish: %#v", m.runningTools)
	}
}

func TestAsyncSubagentResultIsRendered(t *testing.T) {
	input := textarea.New()
	transcript := viewport.New()
	m := &tuiModel{
		width: 80, height: 24, input: input, transcript: transcript,
		cfg: &config{model: "test"}, kernel: &toroid.Kernel{Cfg: toroid.Config{TotalContextSize: 200000}},
	}
	m.resize()

	m.startSubagent("bg-1", "research the API")
	// A running subagent shows the blue animated row but no committed result yet.
	if got := ansi.Strip(m.transcript.View()); !strings.Contains(got, "subagent research the API") {
		t.Fatalf("started subagent row missing:\n%q", got)
	}

	m.stopSubagent("bg-1", "found 3 endpoints\n- /v1/a\n- /v1/b\n- /v1/c")
	got := ansi.Strip(m.transcript.View())
	if len(m.runningSubagents) != 0 {
		t.Fatalf("runningSubagents not cleared: %#v", m.runningSubagents)
	}
	if !strings.Contains(got, "subagent bg-1: found 3 endpoints - /v1/a - /v1/b - /v1/c") {
		t.Fatalf("finished subagent result not rendered:\n%q", got)
	}
}

func TestIntermediateTextPlacedBeforeItsTurnTools(t *testing.T) {
	input := textarea.New()
	transcript := viewport.New()
	m := &tuiModel{
		width: 80, height: 24, input: input, transcript: transcript,
		cfg: &config{model: "test-model"}, kernel: &toroid.Kernel{Cfg: toroid.Config{TotalContextSize: 200000}},
	}
	m.resize()

	// Turn 1: tool commits first, narration arrives after it (real event order).
	m.startTool("id1", "read  foo.go", 1)
	m.finishTool("id1", "")
	m.appendIntermediate("Let me check the file.")
	// Turn 2: another tool, then its narration.
	m.startTool("id2", "edit  bar.go", 2)
	m.finishTool("id2", "")
	m.appendIntermediate("Now I have what I need.")
	// Final answer streams after the last tool.
	m.appendStream("Done.")

	// Assert the entry order, not the padded viewport, so spacing/width don't
	// hide the interleaving.
	var kinds, texts []string
	for _, e := range m.entries {
		kinds = append(kinds, []string{"user", "assistant", "activity", "tool"}[e.kind])
		texts = append(texts, strings.TrimSpace(e.text))
	}
	got := strings.Join(texts, " | ")
	if !strings.Contains(got, "Let me check the file.") || !strings.Contains(got, "Now I have what I need.") {
		t.Fatalf("narration missing: %q", got)
	}
	if !strings.HasPrefix(got, "Let me check the file.") {
		t.Fatalf("turn 1 narration not first: %q", got)
	}
	// Narration must precede the tool of its own turn, and the final answer
	// streams after the last tool.
	if want := "Let me check the file. | read  foo.go | Now I have what I need. | edit  bar.go | Done."; got != want {
		t.Fatalf("entry order = %q, want %q\nkinds=%v", got, want, kinds)
	}
}

func TestNarrationAndToolsReadAsContinuousBlock(t *testing.T) {
	input := textarea.New()
	transcript := viewport.New()
	transcript.SoftWrap = true
	m := &tuiModel{width: 80, height: 40, input: input, transcript: transcript,
		cfg: &config{model: "m"}, kernel: &toroid.Kernel{Cfg: toroid.Config{TotalContextSize: 200000}}}
	m.resize()
	m.addEntry(transcriptUser, "run make cli")
	m.lastNarratedToolNum = 0
	m.startTool("id1", "bash  make cli", 1)
	m.finishTool("id1", "")
	m.appendIntermediate("narration one")
	m.startTool("id2", "bash  ls", 2)
	m.finishTool("id2", "")
	m.startTool("id3", "bash  rm", 3)
	m.finishTool("id3", "")
	m.appendIntermediate("narration two")
	m.appendStream("final")

	got := ansi.Strip(m.transcript.View())
	// Blank lines mark narration<->tool transitions, but consecutive tool rows
	// stay flush. Trim trailing per-line padding (viewport fills to width).
	trimmed := make([]string, 0, len(strings.Split(got, "\n")))
	for _, ln := range strings.Split(got, "\n") {
		trimmed = append(trimmed, strings.TrimRight(ln, " "))
	}
	content := strings.Join(trimmed, "\n")
	// Blank line at narration->tool and tool->narration transitions.
	if !strings.Contains(content, "narration one\n\n1 ⚙ bash  make cli\n\nnarration two\n\n2 ⚙ bash  ls\n3 ⚙ bash  rm\n\nfinal") {
		t.Fatalf("blank-line placement around tools wrong:\n%q", content)
	}
	// Consecutive tool rows stay flush (no blank line between them).
	if !strings.Contains(content, "2 ⚙ bash  ls\n3 ⚙ bash  rm") {
		t.Fatalf("consecutive tool rows not flush:\n%q", content)
	}
}
