package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
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
// "…" while it runs. num is the monotonic call number within the turn.
type tuiToolMsg struct {
	id    string
	label string
	num   int
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

// tuiQueueDrainedMsg announces that messages enqueued while a turn was running
// have been injected at the next turn boundary (EventQueueInterrupt). Each
// message is rendered as a normal user row so it does not silently disappear.
type tuiQueueDrainedMsg struct {
	messages []string
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
	// toolCallSeq is the monotonic counter for tool calls within the current
	// turn, reset to 0 on each new turn.
	toolCallSeq int
	// lastNarratedToolNum is the highest committed tool number whose turn's
	// narration has already been placed. A turn's narration arrives (via
	// TurnCompleted) after its tools are committed, so it must be inserted
	// before that turn's first tool row rather than appended at the end —
	// this tracks how far those insertions have progressed.
	lastNarratedToolNum int
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
}

var (
	canvasColor    = lipgloss.Color("#FFFFFF")
	inputColor     = lipgloss.Color("#FFFFFF")
	userColor      = lipgloss.Color("#ECFDF3")
	inkColor       = lipgloss.Color("#18212F")
	blueStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#3B82F6")).Bold(true)
	accentStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#16834A")).Bold(true)
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#667085"))
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#C24156"))
	errorBoldStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#C24156")).Bold(true)
	boldInkStyle   = lipgloss.NewStyle().Foreground(inkColor).Bold(true)
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
		label := blueStyle.Render("⚡ ") + boldInkStyle.Render("subagent "+id[:min(12, len(id))]+"…")
		if d := formatDuration(time.Since(start)); d != "" {
			label += dimStyle.Render(" · " + d)
		}
		if trimmed := strings.TrimSpace(output); trimmed != "" {
			label += " " + dimStyle.Render(compactToolText(trimmed, 120))
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
		label = label + " ⨯ " + compactToolText(errText, 120)
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
	if d <= 0 {
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
			header := boldInkStyle.Render(m.usernameLabel(entry.userNum) + " >")
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
		if prev == transcriptUser || (r.kind == transcriptTool) != (prev == transcriptTool) {
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

	// Session usage arrives via EventStop for the turn footer's grand-total
	// line; Stream hands the answer to the writer instead of returning a
	// string, so capture the totals here.
	k.On(toroid.EventStop, func(_ context.Context, e toroid.Event) error {
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
		if p, ok := e.Payload.(*toroid.TurnPayload); ok && p.ToolCalls > 0 {
			if text := assistantTextFromTurn(p); text != "" {
				m.events <- tuiIntermediateMsg(text)
			}
		}
		return nil
	})
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			m.toolCallSeq++
			label := toolCallLabel(p.Name, p.Args, m.cfg.workdir)
			m.events <- tuiToolMsg{id: p.CallID, label: label, num: m.toolCallSeq}
		}
		return nil
	})
	k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			m.events <- tuiToolDoneMsg{id: p.CallID}
		}
		return nil
	})
	k.On(toroid.EventPostToolUseFailure, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			m.events <- tuiToolDoneMsg{id: p.CallID, err: p.Error}
		}
		return nil
	})
	k.On(toroid.EventSubagentStart, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok && p.Async {
			m.events <- tuiSubagentMsg{id: p.SessionID, prompt: p.Prompt, start: true, async: true}
		}
		return nil
	})
	k.On(toroid.EventSubagentStop, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok && p.Async {
			m.events <- tuiSubagentMsg{id: p.SessionID, prompt: p.Prompt, output: p.Output, start: false, async: true}
		}
		return nil
	})
	k.On(toroid.EventTurnStarted, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TurnPayload); ok {
			tools := strings.Join(p.Tools, ",")
			if tools == "" {
				tools = "—"
			}
			model := p.Model
			if model == "" {
				model = m.cfg.model
			}
			m.events <- tuiOutputMsg(fmt.Sprintf("→ turn %s · %s · %d msgs · tools: %s", e.TurnID[:min(8, len(e.TurnID))], shortModel(model), p.Messages, tools))
		}
		return nil
	})
	k.On(toroid.EventTurnCompleted, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TurnPayload); ok {
			u := p.TurnUsage
			tok := u.Input + u.CacheRead + u.CacheWrite + u.Output
			if tok > 0 || p.ToolCalls > 0 {
				m.events <- tuiOutputMsg(fmt.Sprintf("← turn %s done · %d tool_calls · %d tokens · $%.4f (total $%.4f) · %s", e.TurnID[:min(8, len(e.TurnID))], p.ToolCalls, tok, p.TurnCostUSD, p.TotalCostUSD, p.StopReason))
			}
		}
		return nil
	})
	k.On(toroid.EventTurnFailed, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TurnPayload); ok {
			m.events <- tuiOutputMsg(fmt.Sprintf("✗ turn %s failed: %s", e.TurnID[:min(8, len(e.TurnID))], p.Error))
		}
		return nil
	})
	k.On(toroid.EventGuardTriggered, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.GuardTriggeredPayload); ok {
			m.events <- tuiOutputMsg(fmt.Sprintf("⚑ guard %s: %s", p.Name, p.Reason))
		}
		return nil
	})
	k.On(toroid.EventPreCompact, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.CompactPayload); ok {
			m.events <- tuiOutputMsg(fmt.Sprintf("◐ compacting · %d msgs · %d tokens", p.MessageCount, p.TokenCount))
		}
		return nil
	})
	k.On(toroid.EventPostCompact, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.CompactSummaryPayload); ok {
			m.events <- tuiOutputMsg(fmt.Sprintf("◑ compacted %d→%d msgs · %d tokens · %s", p.MessagesBefore, p.MessagesAfter, p.TokensBefore, compactToolText(p.Summary, 120)))
		}
		return nil
	})
	k.On(toroid.EventQueueInterrupt, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.QueueInterruptPayload); ok && len(p.Messages) > 0 {
			m.events <- tuiQueueDrainedMsg{messages: p.Messages}
		}
		return nil
	})
}

func (m *tuiModel) submit(prompt string) tea.Cmd {
	m.busy = true
	m.toolCallSeq = 0
	m.lastNarratedToolNum = 0
	m.input.Reset()
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

// delegate runs a /delegate task synchronously in the TUI and returns a done
// message carrying the human-readable summary. It records the task under
// TOROID_HOME like the non-interactive path.
func (m *tuiModel) delegate(prompt string) tea.Cmd {
	m.busy = true
	m.addEntry(transcriptUser, "/delegate "+prompt)
	m.addEntry(transcriptAssistant, "")
	return func() tea.Msg {
		start := time.Now()
		out, err := delegate(m.kernel, prompt)
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
	m.userSeq = 0
	m.toolCallSeq = 0
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
		m.addEntry(transcriptActivity, fmt.Sprintf("cost so far: $%.6f", m.kernel.RunningCostUSD()))
		return nil, true
	case "/model":
		m.addEntry(transcriptActivity, fmt.Sprintf("model: %s · workdir: %s · thinking: %s", m.cfg.model, displayWorkdir(m.cfg.workdir), m.cfg.thinking))
		return nil, true
	case "/delegate":
		m.addEntry(transcriptActivity, "Type /delegate <task> to dispatch a task to another agent")
		return nil, true
	case "/help", "/?":
		m.addEntry(transcriptActivity, "Enter send · Shift+Enter new line · Esc cancel turn · Ctrl+C quit\n/help /cost /model /new /reset /clear /exit /delegate")
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
			// While a turn is already running, queue the new message instead of
			// dropping it: the kernel injects it at the next turn boundary
			// (QueueInterrupt). This replaces the old "busy -> ignore" behavior.
			if m.busy {
				m.input.Reset()
				if strings.HasPrefix(prompt, "/") {
					m.addEntry(transcriptActivity, "can't run commands while busy — finished this turn first")
					return m, nil
				}
				m.addEntry(transcriptUser, prompt)
				m.addEntry(transcriptActivity, "queued — will run after this turn")
				m.kernel.Enqueue(withAgentsReminder(prompt, m.kernel.Cfg.WorkDir))
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
		m.appendStream(string(msg))
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiIntermediateMsg:
		m.appendIntermediate(string(msg))
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiToolMsg:
		m.startTool(msg.id, msg.label, msg.num)
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiToolDoneMsg:
		m.finishTool(msg.id, msg.err)
		cmds = append(cmds, waitTUIEvent(m.events))
	case tuiSubagentMsg:
		if msg.start {
			m.startSubagent(msg.id, msg.prompt)
		} else {
			m.stopSubagent(msg.id, msg.output)
		}
		cmds = append(cmds, waitTUIEvent(m.events))
	case spinnerTick:
		if m.busy {
			m.spinnerFrame++
			m.renderTranscript() // advance the live tool "…" animation too
			cmds = append(cmds, spin())
		}
	case tuiDelegationDoneMsg:
		m.busy = false
		if msg.err != nil {
			m.addEntry(transcriptActivity, "delegation error: "+msg.err.Error())
		} else {
			m.appendAssistant(msg.result)
			if n := len(m.entries); n > 0 && m.entries[n-1].kind == transcriptAssistant {
				m.entries[n-1].done = true
				m.entries[n-1].elapsed = msg.elapsed
				m.entries[n-1].ts = time.Now()
				m.renderTranscript()
				m.transcript.GotoBottom()
			}
		}
	case tuiQueueDrainedMsg:
		// The messages were already shown as user rows at enqueue time; the
		// kernel has now injected them at the turn boundary, so just confirm
		// the flush rather than duplicating the content.
		if n := len(msg.messages); n == 1 {
			m.addEntry(transcriptActivity, "→ queued message injected")
		} else if n > 1 {
			m.addEntry(transcriptActivity, fmt.Sprintf("→ %d queued messages injected", n))
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
	m.transcript, _ = m.transcript.Update(msg)
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

	// USD with 2 decimal places.
	costStr := fmt.Sprintf("$%.2f", cost)

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
	if m.busy {
		statusText = accentStyle.Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]+" working") + dimStyle.Render("  Esc cancel")
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
