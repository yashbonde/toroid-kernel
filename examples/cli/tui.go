package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
	toroid "github.com/yashbonde/toroid-kernel"
)

type tuiOutputMsg string

// tuiStreamMsg carries a chunk of the assistant's answer produced live by
// Stream, so the transcript is updated token-by-turn instead of only after the
// whole agent loop finishes. It is rendered into the assistant entry below.
type tuiStreamMsg string

// tuiIntermediateMsg carries the assistant's written text from a tool-calling
// turn. The kernel streams text to the writer only for the final no-tool turn,
// so this intermediate narration is reconstructed from the TurnCompleted event.
type tuiIntermediateMsg string

// tuiToolMsg announces a tool call that has just started. id is the stable
// tool_call_id from the LLM (links the Pre and Post events); label is the
// single-row display text (operation + trimmed target) shown with an animated
// "…" while it runs. The call is numbered in Update, not here: this message is
// built on the kernel's goroutine, and a background turn firing it concurrently
// with a reset in submit/newSession would race on the counter.
type tuiToolMsg struct {
	id    string
	label string
}

// tuiSubagentMsg announces a subagent start or stop. output is the subagent's
// final text, populated on the stop event; the TUI renders it so a finished
// background agent's result is visible rather than silently discarded.
type tuiSubagentMsg struct {
	id     string // session ID
	prompt string
	output string
	start  bool // true = start, false = stop
	async  bool
}

// tuiToolDoneMsg marks a running tool (by id) as finished. A non-empty err
// carries the failure text; otherwise the call succeeded and a "done!" suffix
// is shown.
type tuiToolDoneMsg struct {
	id  string
	err string
}

// writerFunc adapts a plain func to io.Writer so Stream can push answer text
// into the TUI event loop.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type tuiTurnDoneMsg struct {
	err        error
	elapsed    time.Duration
	grandTotal int64
}

// spinnerTick requests the next animation frame while the kernel is busy.
type spinnerTick struct{}

func spin() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTick{}
	})
}

var spinnerFrames = []string{".  ", ".. ", "...", " ..", "  .", "   "}

type transcriptKind uint8

const (
	transcriptUser transcriptKind = iota
	transcriptAssistant
	transcriptActivity
	transcriptTool
	transcriptBash
	transcriptBashOutput
)

type transcriptEntry struct {
	kind          transcriptKind
	text          string
	rendered      string
	renderedWidth int
	ts            time.Time
	done          bool
	elapsed       time.Duration
	// num is the tool call number for transcriptTool rows (0 for non-tool
	// entries such as subagents) so the number stays visible once a tool's
	// live row is committed on finish.
	num int
	// userNum is the monotonic user-message index shown as "[n]" before the
	// username, so the user can see which stage of the conversation they are on.
	userNum int
}

type tuiModel struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cancelTurn   context.CancelFunc
	kernel       *toroid.Kernel
	cfg          *config
	apiKey       string
	events       chan tea.Msg
	input        textarea.Model
	transcript   viewport.Model
	entries      []transcriptEntry
	username     string
	width        int
	height       int
	busy         bool
	spinnerFrame int
	// runningTools holds every tool call currently in flight, in start order, so
	// concurrent parallel calls each get their own live "…" indicator that
	// animates with the spinner. A tool is removed (and committed to the
	// transcript) when its matching done event arrives.
	runningTools []runningTool
	// runningSubagents tracks in-flight async subagents (sessionID -> start time)
	// so their running indicators show elapsed time with a blue icon.
	runningSubagents map[string]time.Time
	// completedSubagents is a monotonically increasing session counter of
	// subagents that have finished, shown alongside the "ready" status.
	completedSubagents int
	// toolCallSeq is the monotonic counter for tool calls within the current
	// turn, reset to 0 on each new turn.
	toolCallSeq int
	// lastNarratedToolNum is the highest committed tool number whose turn's
	// narration has already been placed. A turn's narration arrives (via
	// TurnCompleted) after its tools are committed, so it must be inserted
	// before that turn's first tool row rather than appended at the end —
	// this tracks how far those insertions have progressed.
	lastNarratedToolNum int
	// streamedNarration accumulates the assistant text delivered live via the
	// stream during the current turn. The kernel also replays a turn's full
	// (accumulated) narration in every TurnCompleted payload, which would
	// otherwise re-insert text the stream already showed; appendIntermediate
	// strips this prefix so each narration renders once. Reset per turn.
	streamedNarration strings.Builder
	// userSeq is the monotonic counter for user messages across the session,
	// shown as "[n]" before the username in the transcript header.
	userSeq int
	// usageMu guards usage, the session-accumulated token usage captured from
	// EventStop for the turn footer's grand-total line.
	usageMu sync.Mutex
	usage   toroid.UsagePayload
	// lastInputHeight is the composer's visual row count from the previous
	// layout pass. With a known terminal width and a monospace font the
	// composer only needs a new height when its content wraps to a new visual
	// row, so the viewport is re-derived only when this actually changes.
	lastInputHeight int
	// sessionTitle is the auto-generated or user-renamed title for the current
	// session, shown in green after the username in the transcript header.
	sessionTitle string
	// bashMode is true while the input starts with '!', so the input area is
	// styled in dark purple and its label reads "bash".
	bashMode bool
	// bashRunning is true while a user-issued '!' command is executing so the
	// status line can show "working" in purple. Unlike bashMode (input-only)
	// and busy (model turn), this tracks the actual in-flight shell command.
	bashRunning bool
	// gen identifies the current kernel generation. /new bumps it so hooks left
	// behind on the replaced kernel stop feeding this TUI.
	gen atomic.Uint64
}

var (
	canvasColor     = lipgloss.Color("#FFFFFF")
	inputColor      = lipgloss.Color("#FFFFFF")
	userColor       = lipgloss.Color("#ECFDF3")
	bashColor       = lipgloss.Color("#EFE9FC")
	inkColor        = lipgloss.Color("#18212F")
	blueStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#3B82F6")).Bold(true)
	accentStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#16834A")).Bold(true)
	greenStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#16834A"))
	purpleStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#7C3AED")).Bold(true)
	purpleDarkStyle = lipgloss.NewStyle().Background(lipgloss.Color("#3B2A63")).Foreground(lipgloss.Color("#FFFFFF"))
	dimStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#667085"))
	errorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#C24156"))
	errorBoldStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#C24156")).Bold(true)
	boldInkStyle    = lipgloss.NewStyle().Foreground(inkColor).Bold(true)
)

func runTUI(cfg config, apiKey string) error {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan tea.Msg, 256)
	k, err := newKernel(ctx, &cfg, apiKey, func(s string) { events <- tuiOutputMsg(s) })
	if err != nil {
		cancel()
		return fmt.Errorf("kernel init: %w", err)
	}
	defer k.Close()
	defer cancel()

	input := textarea.New()
	inputStyles := textarea.DefaultLightStyles()
	styleTextarea := func(s *textarea.StyleState) {
		s.Base = s.Base.Background(inputColor).Foreground(inkColor)
		s.Text = s.Text.Background(inputColor).Foreground(inkColor)
		s.CursorLine = s.CursorLine.Background(inputColor).Foreground(inkColor)
		s.EndOfBuffer = s.EndOfBuffer.Background(inputColor).Foreground(inkColor)
		s.Placeholder = s.Placeholder.Background(inputColor).Foreground(lipgloss.Color("#98A2B3"))
		s.Prompt = s.Prompt.Background(inputColor).Foreground(inkColor)
	}
	styleTextarea(&inputStyles.Focused)
	styleTextarea(&inputStyles.Blurred)
	input.SetStyles(inputStyles)
	input.Placeholder = "Ask Toroid…"
	input.ShowLineNumbers = false
	input.SetHeight(1)
	input.DynamicHeight = true
	input.MinHeight = 1
	input.MaxHeight = 16
	input.MaxContentHeight = 10000
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("shift+enter", "new line"))
	input.Prompt = ""
	input.MaxWidth = 0
	transcript := viewport.New()
	transcript.SoftWrap = false
	transcript.FillHeight = true

	username := "user"
	if out, err := exec.Command("whoami").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		username = strings.TrimSpace(string(out))
	}
	m := &tuiModel{ctx: ctx, cancel: cancel, kernel: k, cfg: &cfg, apiKey: apiKey, events: events, input: input, transcript: transcript, username: username}
	m.subscribeEvents()

	_, err = tea.NewProgram(m).Run()
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

func waitTUIEvent(events <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-events }
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(m.input.Focus(), waitTUIEvent(m.events))
}

func (m *tuiModel) addEntry(kind transcriptKind, text string) {
	entry := transcriptEntry{kind: kind, text: text, ts: time.Now()}
	if kind == transcriptUser {
		m.userSeq++
		entry.userNum = m.userSeq
	}
	m.entries = append(m.entries, entry)
	m.renderTranscript()
	m.transcript.GotoBottom()
}

func (m *tuiModel) appendActivity(text string) {
	text = ansi.Strip(text)
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimLeft(lines[i], " \t")
	}
	text = strings.Join(lines, "\n")
	if n := len(m.entries); n > 0 && m.entries[n-1].kind == transcriptAssistant {
		m.entries[n-1].text += text
		m.entries[n-1].rendered = ""
	} else {
		m.entries = append(m.entries, transcriptEntry{kind: transcriptAssistant, text: text, ts: time.Now()})
	}
	m.renderTranscript()
	m.transcript.GotoBottom()
}

// toolCallMaxLabel caps a tool call to a single display row of at most
// maxToolLabelWidth chars, so chatty operations never span the screen.
const maxToolLabelWidth = 50

// runningTool is one tool call in flight while its handler is executing.
type runningTool struct {
	id    string
	label string
	num   int       // monotonic call number, e.g. 1, 2, 3
	start time.Time // when the running animation started
}

// startTool records a tool call that just began so renderTranscript can draw it
// as a live "<num> <call> …" line with the spinner animating the dots. num is
// the monotonic call counter since the turn started.
func (m *tuiModel) startTool(id, label string, num int) {
	label = clipToolLabel(label)
	for i := range m.runningTools {
		if m.runningTools[i].id == id {
			m.runningTools[i].label = label
			m.runningTools[i].num = num
			m.renderTranscript()
			m.transcript.GotoBottom()
			return
		}
	}
	m.runningTools = append(m.runningTools, runningTool{id: id, label: label, num: num, start: time.Now()})
	m.renderTranscript()
	m.transcript.GotoBottom()
}

// startSubagent tracks an async subagent start so it renders as a blue
// animated row with elapsed time.
func (m *tuiModel) startSubagent(id, prompt string) {
	if m.runningSubagents == nil {
		m.runningSubagents = make(map[string]time.Time)
	}
	m.runningSubagents[id] = time.Now()
	// Commit a permanent entry so the start time anchor is visible.
	label := accentStyle.Render("⚡ ") + boldInkStyle.Render("subagent "+compactToolText(prompt, 60))
	m.entries = append(m.entries, transcriptEntry{kind: transcriptTool, text: label, ts: time.Now()})
	m.renderTranscript()
	m.transcript.GotoBottom()
}

// stopSubagent removes the async subagent from the running set and commits
// a finished transcript row with total time taken and the first line of
// output (if any), so a finished background agent's result is visible.
func (m *tuiModel) stopSubagent(id, output string) {
	start, ok := m.runningSubagents[id]
	delete(m.runningSubagents, id)
	if ok {
		m.completedSubagents++
		// Only ellipsize an id that was actually truncated.
		name := id
		if len(name) > 12 {
			name = name[:12] + "…"
		}
		label := blueStyle.Render("⚡ ") + boldInkStyle.Render("subagent "+name)
		if trimmed := strings.TrimSpace(output); trimmed != "" {
			label += dimStyle.Render(": " + compactToolText(trimmed, 120))
		}
		// Duration trails the row, matching finished tool rows.
		if d := formatDuration(time.Since(start)); d != "" {
			label += dimStyle.Render(" · " + d)
		}
		m.entries = append(m.entries, transcriptEntry{kind: transcriptTool, text: label, ts: time.Now()})
	}
	m.renderTranscript()
	m.transcript.GotoBottom()
}

// clipToolLabel keeps only the first maxToolLabelWidth runes of a tool label,
// replacing anything cut with an ellipsis so the shape stays on one row.
func clipToolLabel(label string) string {
	runes := []rune(label)
	if len(runes) <= maxToolLabelWidth {
		return label
	}
	return string(runes[:maxToolLabelWidth-1]) + "…"
}

// finishTool commits the finished tool (matched by its call id) as a permanent
// transcript row and removes it from the set of running tools. The grey + "…"
// animation made clear it was working; a finished row is solidified in bold
// with total time taken appended after " · <time>".
func (m *tuiModel) finishTool(id, errText string) {
	idx := -1
	for i := range m.runningTools {
		if m.runningTools[i].id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	label := m.runningTools[idx].label
	elapsed := time.Since(m.runningTools[idx].start)
	if errText != "" {
		label = label + " ⨯ " + compactToolText(errText, 80)
	}
	if d := formatDuration(elapsed); d != "" {
		label = label + " … " + d
	}
	num := m.runningTools[idx].num
	m.runningTools = append(m.runningTools[:idx], m.runningTools[idx+1:]...)
	m.entries = append(m.entries, transcriptEntry{kind: transcriptTool, text: label, num: num, ts: time.Now()})
	m.renderTranscript()
	m.transcript.GotoBottom()
}

func (m *tuiModel) appendAssistant(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if n := len(m.entries); n > 0 && m.entries[n-1].kind == transcriptAssistant {
		if strings.TrimSpace(m.entries[n-1].text) != "" {
			m.entries[n-1].text = strings.TrimRight(m.entries[n-1].text, "\n") + "\n\n"
		}
		m.entries[n-1].text += text
		m.entries[n-1].rendered = ""
	} else {
		m.entries = append(m.entries, transcriptEntry{kind: transcriptAssistant, text: text, ts: time.Now()})
	}
	m.renderTranscript()
	m.transcript.GotoBottom()
}

// appendStream appends a live-assistant chunk to the current assistant entry.
// Unlike appendAssistant it preserves the chunk verbatim (no TrimSpace on the
// whole message) so code fences and indentation survive incremental delivery;
// renderTranscript normalizes spacing when it builds the display.
func (m *tuiModel) appendStream(text string) {
	if n := len(m.entries); n > 0 && m.entries[n-1].kind == transcriptAssistant {
		m.entries[n-1].text += text
		m.entries[n-1].rendered = ""
	} else {
		m.entries = append(m.entries, transcriptEntry{kind: transcriptAssistant, text: text, ts: time.Now()})
	}
	m.renderTranscript()
	m.transcript.GotoBottom()
}

// appendIntermediate places the assistant's narration from a tool-calling turn.
// The kernel emits a turn's tools first (PreToolUse/PostToolUse) and its written
// narration afterwards (TurnCompleted), so a narration chunk must be inserted
// before that turn's first committed tool row — otherwise every narration chunk
// would pile up after all the tools, out of the order it was actually said. The
// current turn is delimited by lastNarratedToolNum: narration belongs ahead of
// the first tool row with a higher number.
func (m *tuiModel) appendIntermediate(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Drop the prefix the stream already rendered this turn: the kernel writes
	// tool-turn narration to the writer AND replays the turn's accumulated
	// narration in TurnCompleted, so without this the text between two tool
	// calls would appear twice (and earlier steps' text again after that).
	if streamed := strings.TrimSpace(m.streamedNarration.String()); streamed != "" {
		if strings.HasPrefix(text, streamed) {
			text = strings.TrimSpace(text[len(streamed):])
		} else if strings.Contains(streamed, text) {
			// This payload's narration is already fully covered by what streamed.
			text = ""
		}
		if text == "" {
			return
		}
	}
	entry := transcriptEntry{kind: transcriptAssistant, text: text, ts: time.Now()}
	insertAt := len(m.entries)
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].kind == transcriptTool && m.entries[i].num > m.lastNarratedToolNum {
			insertAt = i
			continue
		}
		break
	}
	m.entries = append(m.entries, transcriptEntry{})
	copy(m.entries[insertAt+1:], m.entries[insertAt:])
	m.entries[insertAt] = entry
	m.lastNarratedToolNum = currentTurnToolMax(m.entries, m.lastNarratedToolNum)
	m.renderTranscript()
	m.transcript.GotoBottom()
}

// currentTurnToolMax returns the highest tool number currently present in the
// transcript, clamped to be no lower than floor. Used to advance
// lastNarratedToolNum so the next narration chunk is inserted after this turn's
// tools.
func currentTurnToolMax(entries []transcriptEntry, floor int) int {
	max := floor
	for i := range entries {
		if entries[i].kind == transcriptTool && entries[i].num > max {
			max = entries[i].num
		}
	}
	return max
}

// timestampLabel renders the grey per-message timestamp suffix.
func timestampLabel(ts time.Time) string {
	return dimStyle.Render(ts.Format("[02/01 15:04]"))
}

// formatDuration renders a compact duration string.
func formatDuration(d time.Duration) string {
	// Sub-millisecond durations round to a meaningless "0ms"; render nothing.
	if d < time.Millisecond {
		return ""
	}
	sec := d.Seconds()
	switch {
	case sec < 1:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case sec < 60:
		return fmt.Sprintf("%.1fs", sec)
	default:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
}

// usernameLabel returns the display label for a user message header, prefixed
// with the monotonic "[n]" index when set (see userNum).
func (m *tuiModel) usernameLabel(num int) string {
	name := m.username
	if name == "" {
		name = "user"
	}
	if num > 0 {
		return fmt.Sprintf("[%d] %s", num, name)
	}
	return name
}

// usernameLine returns the full user header line.
func (m *tuiModel) usernameLine(num int) string {
	return boldInkStyle.Render(m.usernameLabel(num) + " >")
}

func (m *tuiModel) renderTranscript() {
	availableWidth := max(1, m.width)
	type renderedRow struct {
		kind     transcriptKind
		rendered string
	}
	var rendered []renderedRow
	for i := range m.entries {
		entry := &m.entries[i]
		text := strings.TrimSpace(entry.text)
		if text == "" {
			continue
		}
		if entry.rendered != "" && entry.renderedWidth == availableWidth {
			rendered = append(rendered, renderedRow{kind: entry.kind, rendered: entry.rendered})
			continue
		}
		switch entry.kind {
		case transcriptUser:
			text = ansi.Hardwrap(text, availableWidth, false)
			header := m.usernameLine(entry.userNum)
			entry.rendered = lipgloss.NewStyle().Background(userColor).Foreground(inkColor).Width(availableWidth).Render(header + "\n" + text + "\n" + timestampLabel(entry.ts))
		case transcriptAssistant:
			// Do not right-pad assistant/tool lines. The viewport owns blank cells;
			// padding to its exact width can produce a wrapped overflow cell after
			// ANSI style resets in some terminals.
			text = renderAssistantText(text, availableWidth)
			// Only stamp the completion time (and time taken) once the turn has
			// fully answered, so the suffix does not flicker mid-stream.
			body := text
			if entry.done {
				stamp := timestampLabel(entry.ts)
				if d := formatDuration(entry.elapsed); d != "" {
					stamp += "  " + dimStyle.Render("· "+d)
				}
				if body != "" {
					body += "\n\n"
				}
				body += stamp
			}
			entry.rendered = lipgloss.NewStyle().Foreground(inkColor).Render(body)
		case transcriptActivity:
			entry.rendered = dimStyle.Render(renderAssistantText(text, availableWidth))
		case transcriptBash:
			text = ansi.Hardwrap(text, availableWidth, false)
			entry.rendered = lipgloss.NewStyle().Background(bashColor).Foreground(inkColor).Width(availableWidth).Render(purpleStyle.Render("bash> ") + "\n" + text + "\n" + timestampLabel(entry.ts))
		case transcriptBashOutput:
			// Render command output verbatim, preserving newlines/whitespace.
			text = ansi.Hardwrap(text, availableWidth, true)
			entry.rendered = lipgloss.NewStyle().Foreground(inkColor).Width(availableWidth).Render(text + "\n" + timestampLabel(entry.ts))
		case transcriptTool:
			numPrefix := ""
			if entry.num > 0 {
				numPrefix = dimStyle.Render(fmt.Sprintf("%d ", entry.num))
			}
			// Finished async subagents are committed with a pre-styled "⚡ …" label
			// (see stopSubagent); render them verbatim so we keep the blue icon.
			if strings.Contains(text, "⚡") {
				if entry.num > 0 {
					entry.rendered = numPrefix + boldInkStyle.Render(text)
				} else {
					entry.rendered = boldInkStyle.Render(text)
				}
			} else if idx := strings.Index(text, " ⨯ "); idx >= 0 {
				entry.rendered = numPrefix + accentStyle.Render("⚙ ") + errorBoldStyle.Render(text[:idx]) + boldInkStyle.Render(text[idx:])
			} else {
				entry.rendered = numPrefix + accentStyle.Render("⚙ ") + boldInkStyle.Render(text)
			}
		}
		entry.renderedWidth = availableWidth
		rendered = append(rendered, renderedRow{kind: entry.kind, rendered: entry.rendered})
	}
	// Running tool calls are drawn as live animated rows below the settled
	// transcript — one per in-flight call, each with its dots cycling via the
	// spinner frames. Parallel calls all show their own working row. The
	// monotonic call number is shown as a left-aligned prefix.
	for i := range m.runningTools {
		dots := spinnerFrames[(m.spinnerFrame+i)%len(spinnerFrames)]
		elapsed := dimStyle.Render(" · " + formatDuration(time.Since(m.runningTools[i].start)))
		numPrefix := dimStyle.Render(fmt.Sprintf("%d ", m.runningTools[i].num))
		toolLine := numPrefix + accentStyle.Render("⚙ ") + dimStyle.Render(m.runningTools[i].label) + " " + accentStyle.Render(dots) + elapsed
		rendered = append(rendered, renderedRow{kind: transcriptTool, rendered: toolLine})
	}
	// Running async subagents draw as blue animated rows with elapsed time.
	for id, start := range m.runningSubagents {
		dots := spinnerFrames[(m.spinnerFrame+len(m.runningTools))%len(spinnerFrames)]
		elapsed := dimStyle.Render(" · " + formatDuration(time.Since(start)))
		toolLine := blueStyle.Render("⚡ ") + dimStyle.Render("subagent "+shortID(id)) + "… " + blueStyle.Render(dots) + elapsed
		rendered = append(rendered, renderedRow{kind: transcriptTool, rendered: toolLine})
	}
	// Join settled entries: a blank line separates distinct messages (after a
	// user prompt) and marks each transition between narration and a tool box
	// (text->tool or tool->text), while consecutive tool rows stay flush and
	// assistant narration reads as one continuous block.
	var sb strings.Builder
	for i, r := range rendered {
		if i == 0 {
			sb.WriteString(r.rendered)
			continue
		}
		prev := rendered[i-1].kind
		sep := "\n"
		if prev == transcriptUser || prev == transcriptBash || prev == transcriptBashOutput || (r.kind == transcriptTool) != (prev == transcriptTool) || r.kind == transcriptUser || r.kind == transcriptBash {
			sep = "\n\n"
		}
		sb.WriteString(sep)
		sb.WriteString(r.rendered)
	}
	m.transcript.SetContent(sb.String())
}

// renderAssistantText is shared by the interactive viewport and --run
// --plain, making the one-shot command a full, unclipped rendering probe for
// the exact spacing and wrapping users see in the TUI.
func renderAssistantText(text string, width int) string {
	text = normalizeTranscriptSpacing(text)
	if width < 1 {
		return text
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(assistantMarkdownStyle()),
		glamour.WithWordWrap(width),
		glamour.WithTableWrap(true),
	)
	if err != nil {
		return ansi.Hardwrap(text, width, false)
	}
	rendered, err := renderer.Render(text)
	if err != nil {
		return ansi.Hardwrap(text, width, false)
	}
	// Glamour wraps prose itself, while this final width guard handles constructs
	// that have intrinsic widths (long code lines, URLs, and dense tables).
	return ansi.Hardwrap(strings.Trim(rendered, "\n"), width, false)
}

// assistantMarkdownStyle starts with Glamour's complete light syntax theme,
// then removes its document margins and bright banner treatment so Markdown
// feels native to Toroid's restrained, full-width canvas.
func assistantMarkdownStyle() glamouransi.StyleConfig {
	style := styles.LightStyleConfig
	zero := uint(0)
	empty := ""
	ink := "#18212F"
	green := "#16834A"
	muted := "#667085"
	codeSurface := "#F3F4F6"
	bold := true

	style.Document.Margin = &zero
	style.Document.BlockPrefix = empty
	style.Document.BlockSuffix = empty
	style.Document.Color = &ink
	style.Heading.Color = &green
	style.Heading.Bold = &bold
	style.H1.Prefix = empty
	style.H1.Suffix = empty
	style.H1.Color = &green
	style.H1.BackgroundColor = nil
	style.H2.Prefix = "▌ "
	style.H3.Prefix = "› "
	style.H4.Prefix = empty
	style.H5.Prefix = empty
	style.H6.Prefix = empty
	style.BlockQuote.Color = &muted
	style.Code.BackgroundColor = &codeSurface
	style.CodeBlock.Margin = &zero
	style.CodeBlock.BackgroundColor = &codeSurface
	style.DefinitionDescription.BlockPrefix = "  "
	style.Table.CenterSeparator = strPtr("┼")
	style.Table.ColumnSeparator = strPtr("│")
	style.Table.RowSeparator = strPtr("─")
	if style.CodeBlock.Chroma != nil {
		style.CodeBlock.Chroma.Background.BackgroundColor = &codeSurface
	}
	return style
}

func strPtr(value string) *string { return &value }

// normalizeTranscriptSpacing keeps one intentional blank row between
// paragraphs while collapsing the repeated blank lines that can accumulate
// when streamed reasoning, tool events, and the final response are joined.
func normalizeTranscriptSpacing(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			if len(out) > 0 && !blank {
				out = append(out, "")
				blank = true
			}
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func (m *tuiModel) resize() {
	w := max(1, m.width)
	maxInputHeight := max(1, min(16, m.height-8))
	m.input.MinHeight = min(1, maxInputHeight)
	m.input.MaxHeight = maxInputHeight
	// SetWidth recalculates DynamicHeight from visual rows, including soft wraps.
	if m.input.Width() != w {
		m.input.SetWidth(w)
	}
	inputHeight := m.input.Height()
	widthChanged := m.transcript.Width() != w
	if widthChanged {
		m.transcript.SetWidth(w)
	}
	// The viewport only needs a new height when the composer's visual height
	// changed (or the terminal resized). Because the width is known and the
	// font is monospace, the composer height only changes when its content
	// wraps to a new visual row — not per keystroke — so skip the reflow while
	// typing otherwise.
	needsViewportResize := widthChanged || inputHeight != m.lastInputHeight
	m.lastInputHeight = inputHeight
	if needsViewportResize {
		viewportHeight := max(1, m.height-inputHeight-7)
		if m.transcript.Height() != viewportHeight {
			m.transcript.SetHeight(viewportHeight)
		}
	}
	if widthChanged {
		m.renderTranscript()
	}
}

// paintInputSurface makes the editor a rectangular surface instead of relying
// on nested ANSI backgrounds, which only color cells that contain text.
func paintInputSurface(content string, width, height int) string {
	const background = "\x1b[48;2;243;244;246m"
	const reset = "\x1b[0m"
	lines := strings.Split(content, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, line := range lines {
		line = strings.ReplaceAll(line, reset, reset+background)
		if ansi.StringWidth(line) > width {
			line = ansi.Truncate(line, width, "")
		}
		padding := max(0, width-ansi.StringWidth(line))
		lines[i] = background + line + strings.Repeat(" ", padding) + reset
	}
	return strings.Join(lines, "\n")
}

// tuiDelegationDoneMsg marks a /delegate task as finished.
type tuiDelegationDoneMsg struct {
	result  string
	err     error
	elapsed time.Duration
}

// subscribeEvents registers the kernel event hooks once per kernel. It must be
// called whenever the model gets a new kernel (initial start and /new), since
// HookRegistry appends and never unregisters — re-registering per turn would
// stack duplicate hooks and multiply the tool-call counter on later turns.
func (m *tuiModel) subscribeEvents() {
	k := m.kernel
	// Events from a kernel this TUI has since replaced (/new) must be dropped:
	// its hooks still hold this channel, and a delegation outliving the session
	// that started it would otherwise render inside the new one. The generation
	// is atomic because hooks fire on the kernel's goroutine.
	gen := m.gen.Load()
	live := func() bool { return m.gen.Load() == gen }

	// Session usage arrives via EventStop for the turn footer's grand-total
	// line; Stream hands the answer to the writer instead of returning a
	// string, so capture the totals here.
	k.On(toroid.EventStop, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.UsagePayload); ok {
			m.usageMu.Lock()
			m.usage = *p
			m.usageMu.Unlock()
		}
		return nil
	})
	// Reconstruct the assistant's narration from tool-calling turns: the kernel
	// only streams the final no-tool answer to the writer, so intermediate text
	// arrives here via TurnCompleted instead.
	k.On(toroid.EventTurnCompleted, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.TurnPayload); ok && p.ToolCalls > 0 {
			if text := assistantTextFromTurn(p); text != "" {
				m.events <- tuiIntermediateMsg(text)
			}
		}
		return nil
	})
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			m.events <- tuiToolMsg{id: p.CallID, label: toolCallLabel(p.Name, p.Args, m.cfg.workdir)}
		}
		return nil
	})
	k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			m.events <- tuiToolDoneMsg{id: p.CallID}
		}
		return nil
	})
	k.On(toroid.EventPostToolUseFailure, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			m.events <- tuiToolDoneMsg{id: p.CallID, err: p.Error}
		}
		return nil
	})
	k.On(toroid.EventSubagentStart, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok && p.Async {
			m.events <- tuiSubagentMsg{id: p.SessionID, prompt: p.Prompt, start: true, async: true}
		}
		return nil
	})
	k.On(toroid.EventSubagentStop, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok && p.Async {
			m.events <- tuiSubagentMsg{id: p.SessionID, prompt: p.Prompt, output: p.Output, start: false, async: true}
		}
		return nil
	})
	k.On(toroid.EventTurnFailed, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.TurnPayload); ok {
			m.events <- tuiOutputMsg(fmt.Sprintf("✗ failed: %s", p.Error))
		}
		return nil
	})
	k.On(toroid.EventPreCompact, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.CompactPayload); ok {
			m.events <- tuiOutputMsg(fmt.Sprintf("◐ compacting · %d msgs · %d tokens", p.MessageCount, p.TokenCount))
		}
		return nil
	})
	k.On(toroid.EventPostCompact, func(_ context.Context, e toroid.Event) error {
		if !live() {
			return nil
		}
		if p, ok := e.Payload.(*toroid.CompactSummaryPayload); ok {
			m.events <- tuiOutputMsg(fmt.Sprintf("◑ compacted %d→%d msgs · %d tokens · %s", p.MessagesBefore, p.MessagesAfter, p.TokensBefore, compactToolText(p.Summary, 120)))
		}
		return nil
	})
}

func (m *tuiModel) submit(prompt string) tea.Cmd {
	m.busy = true
	m.toolCallSeq = 0
	m.lastNarratedToolNum = 0
	m.streamedNarration.Reset()
	m.input.Reset()
	// Generate a title from the first user prompt.
	if m.userSeq == 0 && m.sessionTitle == "" {
		words := strings.Fields(prompt)
		if len(words) > 6 {
			words = words[:6]
		}
		if title := strings.Join(words, " "); title != "" {
			m.sessionTitle = title
			if m.kernel != nil && m.kernel.Store != nil {
				_ = m.kernel.Store.UpdateTraceTitle(m.kernel.SessionID(), title)
			}
		}
	}
	m.addEntry(transcriptUser, prompt)
	turnCtx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel
	sendPrompt := withAgentsReminder(prompt, m.kernel.Cfg.WorkDir)

	return tea.Batch(
		func() tea.Msg {
			start := time.Now()
			err := m.kernel.Stream(turnCtx, sendPrompt, writerFunc(func(p []byte) (int, error) {
				m.events <- tuiStreamMsg(string(p))
				return len(p), nil
			}))
			var grand int64
			m.usageMu.Lock()
			if total, ok := m.usage.Tokens[m.kernel.SessionID()]; ok {
				grand = total.Input + total.CacheRead + total.CacheWrite + total.Output
			}
			m.usageMu.Unlock()
			return tuiTurnDoneMsg{err: err, elapsed: time.Since(start), grandTotal: grand}
		},
		spin(),
	)
}

// bashSubmit runs a '!' command locally in the shell (workdir) without sending
// it to the model. The command and its output are recorded in the transcript
// purely for the UI.
func (m *tuiModel) bashSubmit(prompt string) tea.Cmd {
	cmd := strings.TrimSpace(strings.TrimPrefix(prompt, "!"))
	m.addEntry(transcriptBash, cmd)
	m.bashRunning = true
	return func() tea.Msg {
		start := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), tuiBashTimeout)
		defer cancel()

		// Source the user's rc so aliases/functions (e.g. gst from ~/.zshrc)
		// resolve, then `eval` the command: alias expansion happens at parse
		// time, so the command must be parsed *after* the rc has run.
		//
		// Deliberately NOT `zsh -i`: an interactive shell attaches to the
		// controlling terminal (job control, tcsetpgrp, tty modes) that the TUI
		// holds in raw mode, and interactive rc's block on /dev/tty prompts
		// (compinit's "Ignore insecure directories?", prompt frameworks). Both
		// freeze the whole UI, not just the command.
		home, _ := os.UserHomeDir()
		inner := "[ -r " + shellQuote(home+"/.zshrc") + " ] && source " + shellQuote(home+"/.zshrc") + " >/dev/null 2>&1; eval \"$TOROID_BASH_CMD\""
		c := exec.CommandContext(ctx, "zsh", "-c", inner)
		c.Dir = m.cfg.workdir
		c.Env = append(os.Environ(), "TOROID_BASH_CMD="+cmd, "PAGER=cat", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "EDITOR=true", "GIT_EDITOR=true")

		// No terminal, ever: empty stdin so anything reading gets EOF instead of
		// waiting, and its own process group so a timeout kills the whole tree
		// (and nothing it spawns can steal the tty from the TUI).
		c.Stdin = nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		c.Cancel = func() error {
			if c.Process != nil {
				_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			}
			return os.ErrProcessDone
		}
		c.WaitDelay = 2 * time.Second

		out, err := c.CombinedOutput()
		res := strings.TrimRight(string(out), "\n")
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			res += fmt.Sprintf("\n(timed out after %s — killed)", tuiBashTimeout)
		case err != nil:
			res += fmt.Sprintf("\n(exit %v)", err)
		}
		return tuiBashDoneMsg{cmd: cmd, out: res, elapsed: time.Since(start)}
	}
}

// tuiBashTimeout bounds a '!' command so a stalled one can never wedge the UI.
const tuiBashTimeout = 120 * time.Second

// tuiBashDoneMsg carries the result of a '!' bash command back to the TUI.
type tuiBashDoneMsg struct {
	cmd     string
	out     string
	elapsed time.Duration
}

// shellQuote quotes s as a single shell word so it can be embedded safely.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// delegate dispatches a /delegate task in the background and returns a done
// message carrying the dispatch summary. The kernel runs the task as a
// background subagent, so this returns as soon as the task is accepted and the
// composer stays live; the result arrives later as a background completion.
// It records the task under TOROID_HOME like the non-interactive path.
func (m *tuiModel) delegate(args string) tea.Cmd {
	prompt, model := parseDelegateArgs(args)
	m.addEntry(transcriptUser, "/delegate "+args)
	return func() tea.Msg {
		start := time.Now()
		out, err := delegate(m.kernel, prompt, model)
		return tuiDelegationDoneMsg{result: out, err: err, elapsed: time.Since(start)}
	}
}

// newSession tears down the current kernel and creates a fresh one with a new
// session id, so /new starts a clean conversation (new transcript, new persisted
// session, history reset) rather than merely clearing history on the same one.
func (m *tuiModel) newSession() {
	if m.cancelTurn != nil {
		m.cancelTurn()
		m.cancelTurn = nil
	}
	_ = m.kernel.Close()
	// Retire the old kernel's hooks before wiring the new one: background tasks
	// it still owns may finish later, and their events belong to a session that
	// no longer exists.
	m.gen.Add(1)
	emit := func(s string) { m.events <- tuiOutputMsg(s) }
	k, err := newKernel(m.ctx, m.cfg, m.apiKey, emit)
	if err != nil {
		m.addEntry(transcriptActivity, "new session failed: "+err.Error())
		return
	}
	m.kernel = k
	m.subscribeEvents()
	m.entries = nil
	m.transcript.SetContent("")
	m.busy = false
	m.runningTools = nil
	m.runningSubagents = nil
	m.completedSubagents = 0
	m.userSeq = 0
	m.toolCallSeq = 0
	m.sessionTitle = ""
	m.addEntry(transcriptActivity, "— new session —")
}

func (m *tuiModel) command(line string) (tea.Cmd, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil, true
	}
	switch strings.ToLower(fields[0]) {
	case "/exit", "/quit", "/q":
		m.cancel()
		return tea.Quit, true
	case "/clear":
		m.entries = nil
		m.transcript.SetContent("")
		return nil, true
	case "/reset":
		m.kernel.History = nil
		m.addEntry(transcriptActivity, "— started a fresh conversation —")
		return nil, true
	case "/new":
		m.newSession()
		return nil, true
	case "/cost":
		m.addEntry(transcriptActivity, fmt.Sprintf("cost so far: ₹%.6f", m.kernel.RunningCostUSD()*100))
		return nil, true
	case "/model":
		m.addEntry(transcriptActivity, fmt.Sprintf("model: %s · workdir: %s · thinking: %s", m.cfg.model, displayWorkdir(m.cfg.workdir), m.cfg.thinking))
		return nil, true
	case "/delegate":
		m.addEntry(transcriptActivity, "Type /delegate [--model <id>] <task> to dispatch a task to another agent (runs in the background)")
		return nil, true
	case "/rename":
		rest := strings.TrimSpace(strings.TrimPrefix(line, "/rename"))
		if rest == "" {
			m.addEntry(transcriptActivity, "usage: /rename <title> — rename the current session (max 50 chars)")
			return nil, true
		}
		runes := []rune(rest)
		if len(runes) > 50 {
			rest = string(runes[:50])
		}
		m.sessionTitle = rest
		if m.kernel != nil && m.kernel.Store != nil {
			_ = m.kernel.Store.UpdateTraceTitle(m.kernel.SessionID(), rest)
		}
		m.addEntry(transcriptActivity, fmt.Sprintf("renamed to %q", rest))
		return nil, true
	case "/help", "/?":
		m.addEntry(transcriptActivity, "Enter send · Shift+Enter new line · Esc cancel turn · Ctrl+C quit\n/help /cost /model /new /reset /clear /exit /delegate /rename")
		return nil, true
	default:
		m.addEntry(transcriptActivity, fmt.Sprintf("unknown command %q — try /help", line))
		return nil, true
	}
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
	case tea.InterruptMsg:
		m.cancel()
		if m.cancelTurn != nil {
			m.cancelTurn()
		}
		return m, tea.Quit
	case tea.KeyPressMsg:
		switch msg.Keystroke() {
		case "ctrl+c":
			m.cancel()
			if m.cancelTurn != nil {
				m.cancelTurn()
			}
			return m, tea.Quit
		case "esc":
			if m.busy && m.cancelTurn != nil {
				m.cancelTurn()
				return m, nil
			}
			if !m.busy && strings.TrimSpace(m.input.Value()) != "" {
				m.input.Reset()
				return m, nil
			}
			// Otherwise let the textarea handle Esc (cursor movement, etc.).
		case "ctrl+d":
			if !m.busy && strings.TrimSpace(m.input.Value()) == "" {
				m.cancel()
				return m, tea.Quit
			}
		case "enter":
			prompt := strings.TrimSpace(m.input.Value())
			if prompt == "" {
				return m, nil
			}
			if m.busy {
				return m, nil
			}
			if strings.HasPrefix(prompt, "/delegate") {
				m.input.Reset()
				prompt = strings.TrimSpace(strings.TrimPrefix(prompt, "/delegate"))
				m.addEntry(transcriptActivity, aGreen+"🛟 delegate identified"+aReset)
				return m, m.delegate(prompt)
			}
			if strings.HasPrefix(prompt, "/") {
				m.input.Reset()
				cmd, _ := m.command(prompt)
				return m, cmd
			}
			if strings.HasPrefix(prompt, "!") {
				m.input.Reset()
				return m, m.bashSubmit(prompt)
			}
			return m, m.submit(prompt)
		case "pgup", "ctrl+up":
			m.transcript.PageUp()
			return m, nil
		case "pgdown", "ctrl+down":
			m.transcript.PageDown()
			return m, nil
		}
	case tuiOutputMsg:
		m.appendActivity(string(msg))
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiStreamMsg:
		m.streamedNarration.WriteString(string(msg))
		m.appendStream(string(msg))
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiIntermediateMsg:
		m.appendIntermediate(string(msg))
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiToolMsg:
		m.toolCallSeq++
		m.startTool(msg.id, msg.label, m.toolCallSeq)
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiToolDoneMsg:
		m.finishTool(msg.id, msg.err)
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiSubagentMsg:
		if msg.start {
			wasEmpty := len(m.runningSubagents) == 0
			m.startSubagent(msg.id, msg.prompt)
			// Kick off the animation tick the first time a subagent becomes
			// in-flight (0 -> N transition). The self-rescheduling spinnerTick
			// below keeps it re-firing until the running set drains, so the
			// row keeps animating even while the main thread is idle.
			if wasEmpty {
				cmds = append(cmds, spin())
			}
		} else {
			m.stopSubagent(msg.id, msg.output)
		}
		cmds = append(cmds, waitTUIEvent(m.events))
	case spinnerTick:
		if m.busy || m.bashRunning || len(m.runningSubagents) > 0 {
			m.spinnerFrame++
			m.renderTranscript() // advance the live tool "…" animation too
			cmds = append(cmds, spin())
		}
	case tuiBashDoneMsg:
		m.bashRunning = false
		if strings.TrimSpace(msg.out) != "" {
			m.addEntry(transcriptBashOutput, msg.out)
		}
	case tuiDelegationDoneMsg:
		// Dispatch only — the task runs in the background. Two consequences: do
		// not clear a busy flag this never set (a turn may be live underneath),
		// and post the acknowledgment as its own entry rather than through
		// appendAssistant, which would merge it into whatever assistant answer
		// happens to be streaming when it lands.
		if msg.err != nil {
			m.addEntry(transcriptActivity, "delegation error: "+msg.err.Error())
		} else {
			m.addEntry(transcriptActivity, msg.result)
		}
	case tuiTurnDoneMsg:
		m.busy = false
		m.cancelTurn = nil
		if msg.err != nil {
			if !errors.Is(msg.err, context.Canceled) {
				m.addEntry(transcriptActivity, "run error: "+msg.err.Error())
			} else {
				m.addEntry(transcriptActivity, "— turn cancelled —")
			}
		} else {
			// The answer streamed into the assistant entry live; just mark it
			// complete and record how long the turn took so the grey
			// timestamp/duration suffix renders after the last chunk.
			if n := len(m.entries); n > 0 && m.entries[n-1].kind == transcriptAssistant {
				m.entries[n-1].done = true
				m.entries[n-1].elapsed = msg.elapsed
				m.entries[n-1].ts = time.Now()
				m.renderTranscript()
				m.transcript.GotoBottom()
			}
		}
	}

	// Keep the composer editable while a turn is running so the user can draft
	// the next message. Plain Enter is intercepted above while busy and cannot
	// submit until the active turn completes.
	var inputCmd tea.Cmd
	m.input, inputCmd = m.input.Update(msg)
	cmds = append(cmds, inputCmd)
	// Toggle bash mode whenever the input starts with '!' so the composer and
	// label restyle live as the user types.
	m.bashMode = strings.HasPrefix(strings.TrimSpace(m.input.Value()), "!")
	// Only forward non-keypress messages to the viewport so left/right arrow
	// keys (which the textarea needs for cursor movement) do not trigger
	// unwanted horizontal scroll. We handle all viewport scrolling explicitly
	// via pgup/pgdown/ctrl+up/ctrl+down.
	if _, ok := msg.(tea.KeyPressMsg); !ok {
		m.transcript, _ = m.transcript.Update(msg)
	}
	m.resize()
	return m, tea.Batch(cmds...)
}

// renderUsageBar builds the bottom status line: USD cost (2dp) + token progress bar.
func (m *tuiModel) renderUsageBar() string {
	used, total := m.kernel.ContextUsage()
	cost := m.kernel.RunningCostUSD()

	// Progress bar: 20 chars wide, filled proportionally.
	barWidth := 20
	filled := 0
	if total > 0 {
		filled = (used * barWidth) / total
		if filled > barWidth {
			filled = barWidth
		}
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	// Rupees with 2 decimal places (USD value x100).
	costStr := fmt.Sprintf("₹%.2f", cost*100)

	// compactNum turns a raw token count into a short human label: 350, 1.2k,
	// 47k, 1.2M, 350M. One decimal is used only in the 1.0k–9.9k and 1.0M–9.9M
	// ranges; otherwise integer kilo/mega with the suffix.
	compact := func(n int) string {
		switch {
		case n < 1000:
			return fmt.Sprintf("%d", n)
		case n < 10_000:
			v := float64(n) / 1000
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", v), "0"), ".") + "k"
		case n < 1_000_000:
			return fmt.Sprintf("%dk", n/1000)
		case n < 10_000_000:
			v := float64(n) / 1_000_000
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", v), "0"), ".") + "M"
		default:
			return fmt.Sprintf("%dM", n/1_000_000)
		}
	}

	tokStr := compact(used) + "/" + compact(total)

	statusText := dimStyle.Render("ready")
	if n := len(m.runningSubagents) + m.completedSubagents; n > 0 {
		statusText += dimStyle.Render(fmt.Sprintf("  ⚡ %d running · %d done", len(m.runningSubagents), m.completedSubagents))
	}
	if m.busy {
		statusText = accentStyle.Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]+" working") + dimStyle.Render("  Esc cancel")
	} else if m.bashRunning {
		statusText = purpleStyle.Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]+" working") + dimStyle.Render("  Esc cancel")
	}

	// Layout: [TOROID model] [path · keys] ... [cost] [progress] [tokens] [status]
	return fmt.Sprintf("%s  %s  %s  %s",
		dimStyle.Render(costStr),
		dimStyle.Render(bar),
		dimStyle.Render(tokStr),
		statusText,
	)
}

func (m *tuiModel) View() tea.View {
	composer := paintInputSurface(m.input.View(), max(1, m.width), m.input.Height())
	headerMeta := dimStyle.Render(displayWorkdir(m.cfg.workdir) + "  ·  PgUp/PgDn scroll  ·  Ctrl+C quit")
	headerMeta = ansi.Truncate(headerMeta, max(1, m.width), "")
	header := accentStyle.Render("TOROID KERNEL INTERFACE") + "  " + boldInkStyle.Render(shortModel(m.cfg.model)) + "\n" + headerMeta
	label := accentStyle.Render(m.username + " >")
	if m.bashMode {
		label = purpleDarkStyle.Render(" bash >")
	} else if m.sessionTitle != "" {
		label += " " + greenStyle.Render(m.sessionTitle)
	}
	usageBar := ansi.Truncate(m.renderUsageBar(), max(1, m.width), "")
	separator := dimStyle.Render(strings.Repeat("─", max(1, m.width)))
	body := header + "\n" + separator + "\n" + m.transcript.View() + "\n" + separator + "\n" + label + "\n" + composer + "\n" + separator + "\n" + usageBar
	body = lipgloss.NewStyle().
		Background(canvasColor).
		Foreground(inkColor).
		Width(max(1, m.width)).
		Height(max(1, m.height)).
		Render(body)
	view := tea.NewView(body)
	view.AltScreen = true
	view.WindowTitle = "Toroid"
	view.BackgroundColor = canvasColor
	view.ForegroundColor = inkColor
	// Leave mouse tracking disabled so the terminal owns drag selection and
	// users can copy transcript text as the raw Markdown displayed on screen.
	// Keyboard scrolling remains available through PgUp/PgDn and Ctrl+Up/Down.
	view.MouseMode = tea.MouseModeNone
	return view
}
