package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

// fakeHost is a stand-in for ToolHost.java on a regular Unix socket (abstract
// sockets are Linux-only). answer turns each request into its response line;
// a nil answer accepts and never replies.
type fakeHost struct {
	path   string
	answer func(op string, args json.RawMessage) string

	mu   sync.Mutex
	reqs []string // raw request lines, newline included
}

func startFakeHost(t *testing.T, answer func(op string, args json.RawMessage) string) *fakeHost {
	t.Helper()
	// t.TempDir is too long for a socket path on macOS (104 bytes).
	dir, err := os.MkdirTemp("", "tc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := &fakeHost{path: filepath.Join(dir, "s"), answer: answer}
	ln, err := net.Listen("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(c)
		}
	}()
	return h
}

func (h *fakeHost) serve(c net.Conn) {
	defer c.Close()
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return
	}
	h.mu.Lock()
	h.reqs = append(h.reqs, line)
	h.mu.Unlock()
	if h.answer == nil {
		_, _ = io.Copy(io.Discard, c) // hold the connection open, silent
		return
	}
	var req struct {
		Op   string          `json:"op"`
		Args json.RawMessage `json:"args"`
	}
	_ = json.Unmarshal([]byte(line), &req)
	_, _ = io.WriteString(c, h.answer(req.Op, req.Args))
}

func (h *fakeHost) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reqs...)
}

func okLine(result string) string { return `{"ok":true,"result":` + result + "}\n" }

func TestToolHostFraming(t *testing.T) {
	h := startFakeHost(t, func(op string, args json.RawMessage) string {
		return okLine(`{"op":"` + op + `","args":` + string(args) + `}`)
	})
	res, err := newToolHost(h.path).call(context.Background(), "task_done", taskIDArgs{ID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"op":"task_done","args":{"id":7}}` {
		t.Fatalf("result = %s", res)
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0] != `{"op":"task_done","args":{"id":7}}`+"\n" {
		t.Fatalf("request lines = %q, want one JSON line ending in a newline", reqs)
	}
	// No args still sends an object, never null.
	if _, err := newToolHost(h.path).call(context.Background(), "tasks_list", nil); err != nil {
		t.Fatal(err)
	}
	if got := h.requests()[1]; got != `{"op":"tasks_list","args":{}}`+"\n" {
		t.Fatalf("request = %q", got)
	}
}

func TestToolHostErrors(t *testing.T) {
	reply := map[string]string{
		"refuse":  `{"ok":false,"error":"no to-do with id 9; call tasks_list for current ids"}` + "\n",
		"blank":   `{"ok":false}` + "\n",
		"garbage": "not json\n",
		"hangup":  "",
	}
	h := startFakeHost(t, func(op string, _ json.RawMessage) string { return reply[op] })
	host := newToolHost(h.path)
	ctx := context.Background()
	for op, want := range map[string]string{
		"refuse":  "no to-do with id 9; call tasks_list for current ids",
		"blank":   "the to-do store refused the request",
		"garbage": "bad reply from the to-do store",
		"hangup":  "closed the connection without answering",
	} {
		_, err := host.call(ctx, op, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", op, err, want)
		}
	}
	_, err := newToolHost(filepath.Join(t.TempDir(), "none")).call(ctx, "tasks_list", nil)
	if !errors.Is(err, errToolHostDown) {
		t.Errorf("no host: err = %v, want errToolHostDown", err)
	}
}

func TestToolHostTimeout(t *testing.T) {
	h := startFakeHost(t, nil) // accepts, never answers
	host := newToolHost(h.path)
	host.timeout = 150 * time.Millisecond
	start := time.Now()
	_, err := host.call(context.Background(), "tasks_list", nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 150ms") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	// Stop (a cancelled request) ends the call at once, not at the timeout.
	host.timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start = time.Now()
	if _, err := host.call(ctx, "tasks_list", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel took %s", d)
	}
}

// runTool calls one registered tool the way the kernel does.
func runTool(t *testing.T, h *toolHost, name, args string) llm.ToolResult {
	t.Helper()
	res, err := todoTools(h).Execute(context.Background(), name, args)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestToolArgsValidation(t *testing.T) {
	h := startFakeHost(t, func(string, json.RawMessage) string { return okLine("{}") })
	host := newToolHost(h.path)
	for _, c := range []struct{ tool, args, want string }{
		{"task_add", `{"title":"Buy milk","due":"2026/11/14"}`, `bad due date "2026/11/14": use YYYY-MM-DD`},
		{"task_add", `{"title":"Buy milk","due":"tomorrow"}`, `bad due date "tomorrow"`},
		{"task_add", `{"title":"Buy milk","due":"2026-02-30"}`, `bad due date "2026-02-30"`},
		{"task_add", `{"title":"  \n "}`, "title is empty"},
		{"task_add", `{"title":"` + strings.Repeat("x", 201) + `"}`, "longer than 200"},
		{"task_add", `{"title":"x","notes":"` + strings.Repeat("n", 2001) + `"}`, "notes are longer"},
		{"tasks_list", `{"from":"14-11-2026"}`, `bad from date`},
		{"tasks_list", `{"from":"2026-11-20","to":"2026-11-14"}`, "is after"},
		{"task_update", `{"id":3}`, "nothing to change"},
		{"task_update", `{"id":3,"due":"Friday"}`, `bad due date "Friday"`},
		{"task_update", `{"id":0,"title":"x"}`, "id must be"},
		{"task_done", `{"id":-1}`, "id must be"},
		{"task_delete", `{}`, "id must be"},
		{"task_done", `{"id":"abc"}`, "invalid arguments"},
	} {
		res := runTool(t, host, c.tool, c.args)
		if !res.IsError || !strings.Contains(res.Content, c.want) {
			t.Errorf("%s %s: result %+v, want an error containing %q", c.tool, c.args, res, c.want)
		}
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("invalid calls reached the host: %q", reqs)
	}
}

func TestToolsForwardToHost(t *testing.T) {
	h := startFakeHost(t, func(op string, args json.RawMessage) string {
		if op == "task_update" {
			return `{"ok":false,"error":"no to-do with id 42; call tasks_list for current ids"}` + "\n"
		}
		return okLine(`{"id":5,"title":"Buy milk","due":"2026-11-14","status":"open"}`)
	})
	host := newToolHost(h.path)
	res := runTool(t, host, "task_add", `{"title":"  Buy\n milk ","due":"2026-11-14"}`)
	if res.IsError || res.Content != `{"id":5,"title":"Buy milk","due":"2026-11-14","status":"open"}` {
		t.Fatalf("task_add result = %+v", res)
	}
	// Titles reach the host on one line; empty optional fields stay out.
	if got := h.requests()[0]; got != `{"op":"task_add","args":{"title":"Buy milk","due":"2026-11-14"}}`+"\n" {
		t.Fatalf("forwarded %q", got)
	}
	// Notes set to "" is a change (clear them), so it is sent; unset fields are not.
	runTool(t, host, "task_update", `{"id":42,"notes":""}`)
	if got := h.requests()[1]; got != `{"op":"task_update","args":{"id":42,"notes":""}}`+"\n" {
		t.Fatalf("forwarded %q", got)
	}
	res = runTool(t, host, "task_update", `{"id":42,"title":"x"}`)
	if !res.IsError || res.Content != "no to-do with id 42; call tasks_list for current ids" {
		t.Fatalf("host refusal = %+v, want its message as an error result", res)
	}
	res = runTool(t, newToolHost(filepath.Join(t.TempDir(), "none")), "tasks_list", `{}`)
	if !res.IsError || !strings.Contains(res.Content, "not reachable") {
		t.Fatalf("no host: %+v", res)
	}
}

func TestToolSchemasHaveTheTools(t *testing.T) {
	r := todoTools(newToolHost("unused"))
	var names []string
	for n, d := range r.Tools() {
		names = append(names, n)
		if d.Description == "" || d.Handler.Parameters()["type"] != "object" {
			t.Errorf("%s: description %q, params %v", n, d.Description, d.Handler.Parameters())
		}
	}
	if len(names) != 5 {
		t.Fatalf("tools = %v", names)
	}
	req, _ := r.Tools()["task_add"].Handler.Parameters()["required"].([]string)
	if !reflect.DeepEqual(req, []string{"title"}) {
		t.Fatalf("task_add required = %v, want only title", req)
	}
}

func TestToolSummary(t *testing.T) {
	year := time.Now().Format("2006")
	for _, c := range []struct{ name, result, err, want string }{
		{"task_add", `{"id":5,"title":"Buy milk","due":"` + year + `-11-14","status":"open"}`, "",
			"✓ added: Buy milk · " + time.Date(time.Now().Year(), 11, 14, 0, 0, 0, 0, time.UTC).Format("Mon 2 Jan")},
		{"task_add", `{"id":5,"title":"Pay rent","due":"2030-01-01","status":"open"}`, "", "✓ added: Pay rent · Tue 1 Jan 2030"},
		{"tasks_list", `[{"id":1},{"id":2},{"id":3}]`, "", "✓ listed 3 to-dos"},
		{"tasks_list", `[{"id":1}]`, "", "✓ listed 1 to-do"},
		{"tasks_list", `[]`, "", "✓ listed 0 to-dos"},
		{"task_done", `{"id":5,"title":"Buy milk","due":"2030-01-01","status":"done"}`, "", "✓ done: Buy milk"},
		{"task_delete", `{"deleted":true,"task":{"id":5,"title":"Buy milk"}}`, "", "✓ deleted: Buy milk"},
		{"task_add", "", "Error: bad due date \"x\": use YYYY-MM-DD", `✗ task_add failed: bad due date "x": use YYYY-MM-DD`},
	} {
		if got := toolSummary(c.name, c.result, c.err); got != c.want {
			t.Errorf("toolSummary(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// toolStep is a scripted model that, on its first turn, says a few words and
// calls task_add; on the next it answers.
type toolStep struct {
	turns int
}

func (s *toolStep) msg() *toroid.AssistantMessage {
	usage := toroid.Usage{Input: 1000, Output: 50, Cost: 0.0001, PricingOK: true}
	if s.turns == 1 {
		return &toroid.AssistantMessage{
			Content: []llm.Part{
				llm.TextPart{Text: "On it."},
				llm.ToolCallPart{ID: "call_1", Name: "task_add", Arguments: `{"title":"Buy milk","due":"2026-11-14"}`},
			},
			Usage: usage, StopReason: toroid.StopReasonToolUse,
		}
	}
	return &toroid.AssistantMessage{Content: []llm.Part{llm.TextPart{Text: "Added: Buy milk, Sat 14 Nov."}}, Usage: usage, StopReason: toroid.StopReasonStop}
}

func (s *toolStep) Complete(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.AssistantMessage, error) {
	s.turns++
	return s.msg(), nil
}

func (s *toolStep) Stream(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.StreamResult, error) {
	s.turns++
	m := s.msg()
	ch := make(chan toroid.StreamDelta, 1)
	ch <- toroid.StreamDelta{Text: llm.TextOf(m.Content)}
	close(ch)
	return &toroid.StreamResult{Deltas: ch, Final: func() (*toroid.AssistantMessage, error) { return m, nil }}, nil
}

func (s *toolStep) CompleteObject(context.Context, toroid.Model, toroid.Context, toroid.Schema, string, string, toroid.StepOptions) (*toroid.ObjectResult, error) {
	return nil, nil
}

// TestChatEndpointRunsTools drives /api/chat with a model that calls task_add
// against a fake tool host: the stream carries the call as tool events, the
// reply text after the call starts a new paragraph, and the saved chat keeps
// the call's summary.
func TestChatEndpointRunsTools(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	t.Setenv(toroid.GatewayBaseURLEnv, "http://127.0.0.1:1")
	h := startFakeHost(t, func(op string, args json.RawMessage) string {
		if op == "tasks_list" {
			return okLine(`[{"id":1,"title":"Call mum","due":"2026-11-13","status":"open"}]`)
		}
		return okLine(`{"id":5,"title":"Buy milk","due":"2026-11-14","status":"open"}`)
	})
	srv := newServer(dir, "tok", 0.5)
	srv.tools = newToolHost(h.path)
	model := defaultModels[0].ID
	sess := srv.sessions.get("s1")
	k, err := srv.newKernelWithKey(context.Background(), model, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if k.SystemPrompt != "" || k.SystemPromptPrefix != chatSystemPrompt {
		t.Fatalf("system prompt = %q", k.SystemPrompt)
	}
	if r := srv.turnReminder(context.Background(), time.Now()); !strings.Contains(r, "Open to-dos: 1.") ||
		!strings.HasPrefix(r, "<system-reminder>\n") || !strings.HasSuffix(r, "\n</system-reminder>") {
		t.Fatalf("reminder = %q", r)
	}
	step := &toolStep{}
	k.Step = step
	sess.kernel, sess.model = k, model

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	body := strings.NewReader(`{"session":"s1","model":"` + model + `","message":"add a todo to buy milk tomorrow"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/chat", body)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var names []string
	var tools []toolEvent
	var text strings.Builder
	for _, e := range parseSSE(t, string(raw)) {
		names = append(names, e.name)
		switch e.name {
		case "tool":
			var te toolEvent
			_ = json.Unmarshal([]byte(e.data), &te)
			tools = append(tools, te)
		case "delta":
			var d textEvent
			_ = json.Unmarshal([]byte(e.data), &d)
			text.WriteString(d.Text)
		case "error":
			t.Fatalf("error event: %s", e.data)
		}
	}
	if got := strings.Join(names, ","); got != "session,prompt,activity,delta,activity,tool,tool,activity,delta,done" {
		t.Fatalf("events = %s\n%s", got, raw)
	}
	const summary = "✓ added: Buy milk · Sat 14 Nov"
	want := []toolEvent{
		{ID: "call_1", Name: "task_add", State: "start", Text: "… adding a to-do"},
		{ID: "call_1", Name: "task_add", State: "done", Text: summary},
	}
	if time.Now().Year() != 2026 {
		want[1].Text = "✓ added: Buy milk · Sat 14 Nov 2026"
	}
	if !reflect.DeepEqual(tools, want) {
		t.Fatalf("tool events = %+v, want %+v", tools, want)
	}
	if text.String() != "On it.\n\nAdded: Buy milk, Sat 14 Nov." {
		t.Fatalf("reply text = %q", text.String())
	}
	// tasks_list for the reminder checked above, again for the message's own
	// reminder, then the model's task_add.
	if reqs := h.requests(); len(reqs) != 3 || !strings.Contains(reqs[1], `"op":"tasks_list"`) || !strings.Contains(reqs[2], `"op":"task_add"`) {
		t.Fatalf("host saw %q, want two reminder tasks_list calls then task_add", reqs)
	}

	rec, err := srv.chats.load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Messages) != 2 {
		t.Fatalf("saved %d messages", len(rec.Messages))
	}
	m := rec.Messages[1]
	if m.Role != "assistant" || !reflect.DeepEqual(m.Tools, []string{want[1].Text}) || m.Text != "On it.\n\nAdded: Buy milk, Sat 14 Nov." {
		t.Fatalf("saved reply = %+v", m)
	}
	// Summaries are for showing the chat, not for the model: a resumed chat's
	// history is the text alone.
	if hist := rec.history(); len(hist) != 2 || hist[1].Text() != m.Text {
		t.Fatalf("rebuilt history = %+v", hist)
	}
}

func TestPromptWithoutToolHost(t *testing.T) {
	srv := newServer(t.TempDir(), "tok", 0.5)
	srv.tools = newToolHost(filepath.Join(t.TempDir(), "none"))
	now := time.Date(2026, 11, 13, 15, 42, 0, 0, time.UTC)
	p := srv.turnReminder(context.Background(), now)
	if !strings.Contains(p, "Friday, 13 November 2026, 15:42") || !strings.Contains(p, "Today is 2026-11-13.") || strings.Contains(p, "Open to-dos") {
		t.Fatalf("prompt = %q", p)
	}
}
