package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// To-dos: one of the agent's two tool groups (the other is Files, files.go).
// The to-do list lives in a local calendar
// named "Toroid" on the phone, one all-day event per to-do, so it also shows in
// the Calendar app. The Go server cannot reach the calendar (that takes a
// ContentResolver and a runtime permission), so ChatService runs a tool host
// (ToolHost.java) on an abstract Unix socket and these tools forward to it:
// one JSON request line, one JSON response line, one connection per call. The
// host only answers peers with the app's own uid, which this server, its child
// process, has.

// toolSocket is the tool host's address. A leading "@" is Linux's abstract
// namespace, where android.net.LocalServerSocket listens.
const toolSocket = "@toroidchat-tools"

const (
	toolTimeout  = 10 * time.Second // one tool call, dial to reply
	countTimeout = 2 * time.Second  // the open-to-do count in a new kernel's prompt
	maxReplySize = 1 << 20          // bytes in one reply line
	maxTitleLen  = 200              // runes in a to-do title
	maxNotesLen  = 2000             // runes in a to-do's notes
)

// toolHost is the Go end of the tool host protocol.
type toolHost struct {
	addr    string // Unix socket path, or "@name" for an abstract socket
	timeout time.Duration
}

func newToolHost(addr string) *toolHost { return &toolHost{addr: addr, timeout: toolTimeout} }

type toolRequest struct {
	Op   string `json:"op"`
	Args any    `json:"args"`
}

type toolResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// errToolHostDown is returned when the tool host cannot be reached: the app's
// service is not running, or this is a laptop run with no phone behind it.
// errFileHostDown is the same for the file tools.
var (
	errToolHostDown = errors.New("the phone's to-do store is not reachable (is Toroid Chat's service running?)")
	errFileHostDown = errors.New("the phone's file store is not reachable (is Toroid Chat's service running?)")
)

// storeName is what the host is to the model in an error about op: the file
// tools reach the phone's files, every other op its to-do list.
func storeName(op string) string {
	if isFileOp(op) {
		return "file store"
	}
	return "to-do store"
}

// call sends one op and returns its result. A refusal from the host (a missing
// permission, an unknown id) comes back as an error carrying the host's own
// message, which is written for the model to read.
func (h *toolHost) call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", h.addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, h.ctxError(ctx, op)
		}
		if isFileOp(op) {
			return nil, errFileHostDown
		}
		return nil, errToolHostDown
	}
	defer conn.Close()
	// The deadline bounds a host that accepts but never answers; AfterFunc
	// also unblocks the read when the user presses Stop.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if args == nil {
		args = struct{}{}
	}
	req, err := json.Marshal(toolRequest{Op: op, Args: args})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, h.ioError(ctx, op, err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxReplySize)).ReadBytes('\n')
	if err != nil {
		return nil, h.ioError(ctx, op, err)
	}
	var resp toolResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("bad reply from the %s: %v", storeName(op), err)
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "the " + storeName(op) + " refused the request"
		}
		return nil, errors.New(resp.Error)
	}
	if len(resp.Result) == 0 {
		resp.Result = json.RawMessage("null")
	}
	return resp.Result, nil
}

// ctxError explains a call that ran out of time or was cancelled. The conn's
// deadline can pass a moment before ctx reports it, so anything but a
// cancellation is the timeout.
func (h *toolHost) ctxError(ctx context.Context, op string) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return fmt.Errorf("the phone's %s did not answer within %s", storeName(op), h.timeout)
}

func (h *toolHost) ioError(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) {
		return h.ctxError(ctx, op)
	}
	if errors.Is(err, io.EOF) {
		return errors.New("the " + storeName(op) + " closed the connection without answering")
	}
	return fmt.Errorf("talking to the %s: %v", storeName(op), err)
}

// openCount is the number of open to-dos, for a new kernel's system prompt.
func (h *toolHost) openCount(ctx context.Context) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, countTimeout)
	defer cancel()
	raw, err := h.call(ctx, "tasks_list", struct{}{})
	if err != nil {
		return 0, false
	}
	var list []todo
	if json.Unmarshal(raw, &list) != nil {
		return 0, false
	}
	return len(list), true
}

// todo is one to-do as the tool host returns it.
type todo struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Due    string `json:"due"` // YYYY-MM-DD
	Status string `json:"status"`
	Notes  string `json:"notes,omitempty"`
}

// Tool arguments. Dates are validated here so a bad one is answered without a
// round trip to the phone; the host checks them again.

type tasksListArgs struct {
	IncludeDone bool   `json:"include_done,omitempty" jsonschema:"description=Also return to-dos already marked done (default false: open to-dos only)"`
	From        string `json:"from,omitempty" jsonschema:"description=Only to-dos due on or after this date (YYYY-MM-DD)"`
	To          string `json:"to,omitempty" jsonschema:"description=Only to-dos due on or before this date (YYYY-MM-DD)"`
}

type taskAddArgs struct {
	Title string `json:"title" jsonschema:"description=What to do, short (e.g. Buy milk),minLength=1,maxLength=200"`
	Due   string `json:"due,omitempty" jsonschema:"description=Due date (YYYY-MM-DD); default today"`
	Notes string `json:"notes,omitempty" jsonschema:"description=Optional extra details,maxLength=2000"`
}

type taskUpdateArgs struct {
	ID    int64   `json:"id" jsonschema:"description=The to-do's id from tasks_list or task_add,minimum=1"`
	Title *string `json:"title,omitempty" jsonschema:"description=New title,minLength=1,maxLength=200"`
	Due   *string `json:"due,omitempty" jsonschema:"description=New due date (YYYY-MM-DD)"`
	Notes *string `json:"notes,omitempty" jsonschema:"description=New notes; an empty string clears them,maxLength=2000"`
}

type taskIDArgs struct {
	ID int64 `json:"id" jsonschema:"description=The to-do's id from tasks_list or task_add,minimum=1"`
}

func checkDate(field, v string) error {
	if _, err := time.Parse(time.DateOnly, v); err != nil {
		return fmt.Errorf("bad %s date %q: use YYYY-MM-DD", field, v)
	}
	return nil
}

// cleanTitle puts a title on one line and checks its length.
func cleanTitle(t string) (string, error) {
	t = strings.Join(strings.Fields(t), " ")
	if t == "" {
		return "", errors.New("title is empty")
	}
	if utf8.RuneCountInString(t) > maxTitleLen {
		return "", fmt.Errorf("title is longer than %d characters", maxTitleLen)
	}
	return t, nil
}

func checkNotes(n string) error {
	if utf8.RuneCountInString(n) > maxNotesLen {
		return fmt.Errorf("notes are longer than %d characters", maxNotesLen)
	}
	return nil
}

func checkID(id int64) error {
	if id <= 0 {
		return errors.New("id must be a to-do id from tasks_list or task_add")
	}
	return nil
}

func (a *tasksListArgs) validate() error {
	for _, d := range []struct{ field, v string }{{"from", a.From}, {"to", a.To}} {
		if d.v != "" {
			if err := checkDate(d.field, d.v); err != nil {
				return err
			}
		}
	}
	if a.From != "" && a.To != "" && a.From > a.To {
		return fmt.Errorf("from %s is after to %s", a.From, a.To)
	}
	return nil
}

func (a *taskAddArgs) validate() (err error) {
	if a.Title, err = cleanTitle(a.Title); err != nil {
		return err
	}
	if a.Due != "" {
		if err := checkDate("due", a.Due); err != nil {
			return err
		}
	}
	return checkNotes(a.Notes)
}

func (a *taskUpdateArgs) validate() error {
	if err := checkID(a.ID); err != nil {
		return err
	}
	if a.Title == nil && a.Due == nil && a.Notes == nil {
		return errors.New("nothing to change: give a title, due or notes")
	}
	if a.Title != nil {
		t, err := cleanTitle(*a.Title)
		if err != nil {
			return err
		}
		a.Title = &t
	}
	if a.Due != nil {
		if err := checkDate("due", *a.Due); err != nil {
			return err
		}
	}
	if a.Notes != nil {
		return checkNotes(*a.Notes)
	}
	return nil
}

func (a *taskIDArgs) validate() error { return checkID(a.ID) }

// validator is every args struct above.
type validator interface{ validate() error }

// hostTool is a tool that validates its args, then forwards them to the host
// under the tool's own name, returning the host's result JSON to the model.
func hostTool[T any, PT interface {
	*T
	validator
}](h *toolHost, name, desc string) *tools.ToolDef {
	return &tools.ToolDef{
		Name:        name,
		Description: desc,
		Handler: llm.NewTool(name, desc, func(ctx context.Context, args T) (llm.ToolResult, error) {
			if err := PT(&args).validate(); err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			res, err := h.call(ctx, name, &args)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return llm.NewTextResult(string(res)), nil
		}),
	}
}

// todoTools is the five to-do tools.
func todoTools(h *toolHost) *tools.Registry {
	r := tools.NewRegistry()
	r.Register(hostTool[tasksListArgs](h, "tasks_list",
		"List the user's to-dos (id, title, due date, status, notes), soonest first. "+
			"By default only open to-dos, any date; set include_done for done ones too, and from/to to limit the due dates."))
	r.Register(hostTool[taskAddArgs](h, "task_add",
		"Add a to-do to the user's list. It is saved as an all-day event on its due date in the Toroid calendar. Returns the new to-do with its id."))
	r.Register(hostTool[taskUpdateArgs](h, "task_update",
		"Change a to-do's title, due date or notes. Give only the fields to change. Returns the updated to-do."))
	r.Register(hostTool[taskIDArgs](h, "task_done",
		"Mark a to-do as done. It stays in the calendar with a ✓ before its title. Returns the updated to-do."))
	r.Register(hostTool[taskIDArgs](h, "task_delete",
		"Delete a to-do for good. Use when the user asks to remove one, not to finish it. Returns the deleted to-do."))
	return r
}

// agentTools is the agent's whole tool set: the to-do tools and the file
// tools (files.go), and nothing else.
func agentTools(h *toolHost) *tools.Registry {
	r := todoTools(h)
	registerFileTools(r, h)
	return r
}

// toolStartText is the line the UI shows while a tool runs.
func toolStartText(name string) string {
	if isFileOp(name) {
		return fileStartText(name)
	}
	if s, ok := appStartText(name); ok {
		return s
	}
	if s, ok := imageStartText(name); ok {
		return s
	}
	switch name {
	case "tasks_list":
		return "… listing to-dos"
	case "task_add":
		return "… adding a to-do"
	case "task_update":
		return "… updating a to-do"
	case "task_done":
		return "… marking a to-do done"
	case "task_delete":
		return "… deleting a to-do"
	}
	return "… " + name
}

// toolSummary is the one line that stands for a finished tool call, in the UI
// and in the saved chat: "✓ added: Buy milk · Sat 14 Nov", "✓ listed 3
// to-dos", or "✗ task_add failed: …". result is the tool's text as the model
// saw it; errText is set when the call failed.
func toolSummary(name, result, errText string) string {
	if errText != "" {
		msg := strings.TrimSpace(strings.TrimPrefix(errText, "Error:"))
		return "✗ " + name + " failed: " + oneLine(msg, 120)
	}
	if isFileOp(name) {
		return fileSummary(name, result)
	}
	if s, ok := appSummary(name, result); ok {
		return s
	}
	if s, ok := imageSummary(name, result); ok {
		return s
	}
	if name == "tasks_list" {
		var list []todo
		if json.Unmarshal([]byte(result), &list) == nil {
			if len(list) == 1 {
				return "✓ listed 1 to-do"
			}
			return fmt.Sprintf("✓ listed %d to-dos", len(list))
		}
		return "✓ listed to-dos"
	}
	var t todo
	if name == "task_delete" {
		var del struct {
			Task todo `json:"task"`
		}
		_ = json.Unmarshal([]byte(result), &del)
		t = del.Task
	} else {
		_ = json.Unmarshal([]byte(result), &t)
	}
	verb := map[string]string{"task_add": "added", "task_update": "updated", "task_done": "done", "task_delete": "deleted"}[name]
	if verb == "" {
		return "✓ " + name
	}
	if t.Title == "" {
		return "✓ " + verb
	}
	line := "✓ " + verb + ": " + oneLine(t.Title, 80)
	if (name == "task_add" || name == "task_update") && t.Due != "" {
		line += " · " + shortDate(t.Due, time.Now())
	}
	return line
}

// shortDate is "Sat 14 Nov", with the year when it is not now's year.
func shortDate(day string, now time.Time) string {
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	if d.Year() != now.Year() {
		return d.Format("Mon 2 Jan 2006")
	}
	return d.Format("Mon 2 Jan")
}

// oneLine collapses s to one line of at most n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
