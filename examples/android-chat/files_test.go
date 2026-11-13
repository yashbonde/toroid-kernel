package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

// runAgentTool calls one tool of the agent's whole set the way the kernel does.
func runAgentTool(t *testing.T, h *toolHost, name, args string) llm.ToolResult {
	t.Helper()
	res, err := agentTools(h).Execute(context.Background(), name, args)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAgentToolsHaveBothGroups(t *testing.T) {
	r := agentTools(newToolHost("unused"))
	var names []string
	for n, d := range r.Tools() {
		names = append(names, n)
		if d.Description == "" || d.Handler.Parameters()["type"] != "object" {
			t.Errorf("%s: description %q, params %v", n, d.Description, d.Handler.Parameters())
		}
	}
	if len(names) != 10 {
		t.Fatalf("tools = %v, want the 5 to-do and 5 file tools", names)
	}
	for _, n := range []string{"files_list", "file_save", "file_read", "file_open", "file_delete", "task_add"} {
		if _, ok := r.Tools()[n]; !ok {
			t.Errorf("missing %s", n)
		}
	}
	req, _ := r.Tools()["file_save"].Handler.Parameters()["required"].([]string)
	if !reflect.DeepEqual(req, []string{"name", "content"}) {
		t.Fatalf("file_save required = %v, want name and content", req)
	}
}

func TestFileToolsForwardToHost(t *testing.T) {
	h := startFakeHost(t, func(op string, args json.RawMessage) string {
		switch op {
		case "file_save":
			return okLine(`{"name":"Packing list.html","path":"Documents/Toroid/Packing list.html","bytes":2100}`)
		case "file_open":
			return `{"ok":false,"error":"no app on this phone opens application/pdf"}` + "\n"
		}
		return okLine(`[]`)
	})
	host := newToolHost(h.path)
	res := runAgentTool(t, host, "file_save", `{"name":"  Packing\n list ","content":"<!doctype html><title>x</title>"}`)
	if res.IsError || !strings.Contains(res.Content, `"bytes":2100`) {
		t.Fatalf("file_save result = %+v", res)
	}
	// The name reaches the host tidied, with the default .html; format and an
	// unset overwrite stay out. (encoding/json escapes < and > as \u003c and
	// \u003e.)
	want := strings.NewReplacer("<", `\u003c`, ">", `\u003e`).Replace(
		`{"op":"file_save","args":{"name":"Packing list.html","content":"<!doctype html><title>x</title>"}}`) + "\n"
	if got := h.requests()[0]; got != want {
		t.Fatalf("forwarded %q\nwant      %q", got, want)
	}
	runAgentTool(t, host, "file_save", `{"name":"notes","content":"# hi","format":"markdown","overwrite":true}`)
	if got := h.requests()[1]; got != `{"op":"file_save","args":{"name":"notes.md","content":"# hi","overwrite":true}}`+"\n" {
		t.Fatalf("forwarded %q", got)
	}
	runAgentTool(t, host, "files_list", `{}`)
	runAgentTool(t, host, "file_read", `{"name":"notes.md"}`)
	runAgentTool(t, host, "file_delete", `{"name":"old.txt"}`)
	reqs := h.requests()
	for i, w := range []string{
		`{"op":"files_list","args":{}}`,
		`{"op":"file_read","args":{"name":"notes.md"}}`,
		`{"op":"file_delete","args":{"name":"old.txt"}}`,
	} {
		if reqs[2+i] != w+"\n" {
			t.Errorf("request %d = %q, want %q", 2+i, reqs[2+i], w)
		}
	}
	res = runAgentTool(t, host, "file_open", `{"name":"scan.pdf"}`)
	if !res.IsError || res.Content != "no app on this phone opens application/pdf" {
		t.Fatalf("host refusal = %+v, want its message as an error result", res)
	}
	res = runAgentTool(t, newToolHost(filepath.Join(t.TempDir(), "none")), "files_list", `{}`)
	if !res.IsError || !strings.Contains(res.Content, "file store is not reachable") {
		t.Fatalf("no host: %+v", res)
	}
}

func TestFileHostErrorsNameTheFileStore(t *testing.T) {
	h := startFakeHost(t, func(string, json.RawMessage) string { return `{"ok":false}` + "\n" })
	_, err := newToolHost(h.path).call(context.Background(), "file_open", nil)
	if err == nil || err.Error() != "the file store refused the request" {
		t.Fatalf("err = %v", err)
	}
	_, err = newToolHost(filepath.Join(t.TempDir(), "none")).call(context.Background(), "file_read", nil)
	if !errors.Is(err, errFileHostDown) {
		t.Fatalf("no host: err = %v, want errFileHostDown", err)
	}
}

func TestFileNameValidation(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"notes.md", "notes.md"},
		{"  Wedding   checklist.html ", "Wedding checklist.html"},
		{"Report (final)_v2-b.txt", "Report (final)_v2-b.txt"},
		{"photo.JPG", "photo.JPG"},
	} {
		got, err := checkFileName(c.in)
		if err != nil || got != c.want {
			t.Errorf("checkFileName(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"", "name is empty"},
		{"   ", "name is empty"},
		{"../x.txt", "no folders"},
		{"..", `no ".."`},
		{"a..txt", `no ".."`},
		{"sub/notes.txt", "no folders"},
		{`sub\notes.txt`, "no folders"},
		{"/sdcard/Download/x.txt", "no folders"},
		{".hidden.txt", "cannot start with a dot"},
		{".bashrc", "cannot start with a dot"},
		{"notes", "needs an extension"},
		{strings.Repeat("a", 81) + ".txt", "80 characters"},
		{"café.txt", "letters, digits"},
		{"a;rm.txt", "letters, digits"},
		{"tab\x00.txt", "letters, digits"},
		{"-x.txt", ""}, // a leading dash is fine
	} {
		_, err := checkFileName(c.in)
		if c.want == "" {
			if err != nil {
				t.Errorf("checkFileName(%q) = %v, want ok", c.in, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("checkFileName(%q) = %v, want an error containing %q", c.in, err, c.want)
		}
	}
	if _, err := checkFileName(strings.Repeat("a", 80) + ".html"); err != nil {
		t.Errorf("80-character name: %v", err)
	}
}

func TestSaveNameFormats(t *testing.T) {
	for _, c := range []struct{ name, format, want string }{
		{"Packing list", "", "Packing list.html"},      // a document is HTML by default
		{"Packing list.html", "", "Packing list.html"}, // already named
		{"Packing list", "html", "Packing list.html"},  //
		{"notes", "md", "notes.md"},                    //
		{"notes", "Markdown", "notes.md"},              // an alias
		{"notes", ".txt", "notes.txt"},                 // a leading dot
		{"notes", "text", "notes.txt"},                 //
		{"notes.md", "markdown", "notes.md"},           // ext and format agree
		{"page.htm", "html", "page.htm"},               //
		{"data.json", "", "data.json"},                 //
		{"Notes v1.2", "", "Notes v1.2.html"},          // ".2" is not an extension
		{"Budget 2026", "csv", "Budget 2026.csv"},      //
		{"script.PY", "", "script.PY"},                 // case kept, matched lower-case
		{"trailing.", "txt", "trailing.txt"},           // trailing dots dropped
		{"config.yml", "yaml", "config.yml"},           //
	} {
		got, err := saveName(c.name, c.format)
		if err != nil || got != c.want {
			t.Errorf("saveName(%q, %q) = %q, %v; want %q", c.name, c.format, got, err, c.want)
		}
	}
	for _, c := range []struct{ name, format, want string }{
		{"notes.md", "txt", "make them agree"},
		{"photo.jpg", "", "not a text format"},
		{"scan.pdf", "", "not a text format"},
		{"notes", "pdf", "text formats only"},
		{"notes", "docx", "text formats only"},
		{"", "md", "name is empty"},
		{"../etc/passwd", "txt", "no folders"},
		{".profile", "txt", "cannot start with a dot"},
	} {
		_, err := saveName(c.name, c.format)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("saveName(%q, %q) = %v, want an error containing %q", c.name, c.format, err, c.want)
		}
	}
}

func TestFileArgsValidation(t *testing.T) {
	h := startFakeHost(t, func(string, json.RawMessage) string { return okLine("{}") })
	host := newToolHost(h.path)
	big := strings.Repeat("x", maxSaveBytes+1)
	for _, c := range []struct{ tool, args, want string }{
		{"file_save", `{"name":"../x.txt","content":"hi"}`, "no folders"},
		{"file_save", `{"name":"x.txt","content":"  \n "}`, "content is empty"},
		{"file_save", `{"name":"x.txt"}`, "content is empty"},
		{"file_save", `{"name":"x.txt","content":"` + big + `"}`, "at most 1 MB"},
		{"file_save", `{"name":"x","content":"hi","format":"exe"}`, "text formats only"},
		{"file_save", `{"name":".x","content":"hi"}`, "cannot start with a dot"},
		{"file_read", `{"name":"/etc/hosts.txt"}`, "no folders"},
		{"file_read", `{}`, "name is empty"},
		{"file_open", `{"name":"..\\x.html"}`, "no folders"},
		{"file_open", `{"name":"notes"}`, "needs an extension"},
		{"file_delete", `{"name":"../../Download/a.txt"}`, "no folders"},
		{"file_delete", `{"name":42}`, "invalid arguments"},
	} {
		res := runAgentTool(t, host, c.tool, c.args)
		if !res.IsError || !strings.Contains(res.Content, c.want) {
			t.Errorf("%s %s: result %+v, want an error containing %q", c.tool, oneLine(c.args, 60), res, c.want)
		}
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("invalid calls reached the host: %q", reqs)
	}
}

func TestFileSummary(t *testing.T) {
	for _, c := range []struct{ name, result, err, want string }{
		{"file_save", `{"name":"Wedding checklist.html","path":"Documents/Toroid/Wedding checklist.html","bytes":2100}`, "",
			"✓ saved: Wedding checklist.html (2 KB) · Documents/Toroid"},
		{"file_save", `{"name":"a.txt","bytes":12}`, "", "✓ saved: a.txt (12 B) · Documents/Toroid"},
		{"file_save", `{"name":"big.json","bytes":1468006}`, "", "✓ saved: big.json (1.4 MB) · Documents/Toroid"},
		{"file_open", `{"name":"notes.md","opened_as":"text"}`, "", "✓ opened: notes.md as text"},
		{"file_open", `{"name":"Packing list.html","opened_as":"a web page"}`, "", "✓ opened: Packing list.html as a web page"},
		{"files_list", `[{"name":"a"},{"name":"b"},{"name":"c"}]`, "", "✓ listed 3 files"},
		{"files_list", `[{"name":"a"}]`, "", "✓ listed 1 file"},
		{"files_list", `[]`, "", "✓ listed 0 files"},
		{"file_read", `{"name":"notes.md","bytes":3000,"content":"..."}`, "", "✓ read: notes.md (3 KB)"},
		{"file_read", `{"name":"log.txt","bytes":65536,"truncated":true}`, "", "✓ read: log.txt (first 64 KB)"},
		{"file_delete", `{"deleted":true,"name":"old.txt"}`, "", "✓ deleted: old.txt"},
		{"file_open", "", "Error: no app on this phone opens application/pdf", "✗ file_open failed: no app on this phone opens application/pdf"},
	} {
		if got := toolSummary(c.name, c.result, c.err); got != c.want {
			t.Errorf("toolSummary(%s) = %q, want %q", c.name, got, c.want)
		}
	}
	for name, want := range map[string]string{
		"files_list": "… listing files", "file_save": "… saving a file", "file_open": "… opening a file",
		"file_read": "… reading a file", "file_delete": "… deleting a file", "task_add": "… adding a to-do",
	} {
		if got := toolStartText(name); got != want {
			t.Errorf("toolStartText(%s) = %q, want %q", name, got, want)
		}
	}
}

// saveStep is a scripted model that, on its first turn, calls file_save with
// a small HTML document; on the next it answers.
type saveStep struct{ toolStep }

const savedDoc = `<!doctype html><html><head><title>Beach packing</title><style>body{font:16px sans-serif}</style></head>` +
	`<body><h1>Beach packing</h1><ul><li>Sunscreen</li><li>Towel</li><li>Hat</li></ul></body></html>`

func (s *saveStep) msg() *toroid.AssistantMessage {
	usage := toroid.Usage{Input: 1000, Output: 50, Cost: 0.0001, PricingOK: true}
	if s.turns == 1 {
		args, _ := json.Marshal(map[string]string{"name": "Beach packing", "content": savedDoc})
		return &toroid.AssistantMessage{
			Content: []llm.Part{llm.ToolCallPart{ID: "call_1", Name: "file_save", Arguments: string(args)}},
			Usage:   usage, StopReason: toroid.StopReasonToolUse,
		}
	}
	return &toroid.AssistantMessage{Content: []llm.Part{llm.TextPart{Text: "Saved Beach packing.html in Documents › Toroid."}}, Usage: usage, StopReason: toroid.StopReasonStop}
}

func (s *saveStep) Complete(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.AssistantMessage, error) {
	s.turns++
	return s.msg(), nil
}

func (s *saveStep) Stream(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.StreamResult, error) {
	s.turns++
	m := s.msg()
	ch := make(chan toroid.StreamDelta, 1)
	ch <- toroid.StreamDelta{Text: llm.TextOf(m.Content)}
	close(ch)
	return &toroid.StreamResult{Deltas: ch, Final: func() (*toroid.AssistantMessage, error) { return m, nil }}, nil
}

// TestChatEndpointSavesFile drives /api/chat with a model that calls
// file_save against a fake tool host: the stream carries the call as tool
// events, the host gets the document under its .html name, and the saved chat
// keeps the call's summary.
func TestChatEndpointSavesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	t.Setenv(toroid.GatewayBaseURLEnv, "http://127.0.0.1:1")
	var saved struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	h := startFakeHost(t, func(op string, args json.RawMessage) string {
		switch op {
		case "tasks_list":
			return okLine(`[]`)
		case "file_save":
			_ = json.Unmarshal(args, &saved)
			return okLine(`{"name":"Beach packing.html","path":"Documents/Toroid/Beach packing.html","bytes":2048}`)
		}
		return `{"ok":false,"error":"unexpected op"}` + "\n"
	})
	srv := newServer(dir, "tok", 0.5)
	srv.tools = newToolHost(h.path)
	model := defaultModels[0].ID
	sess := srv.sessions.get("f1")
	k, err := srv.newKernelWithKey(context.Background(), model, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.SystemPromptPrefix, "Documents › Toroid") || !strings.Contains(k.SystemPromptPrefix, "self-contained HTML document") {
		t.Fatalf("system prompt prefix has no Files rules: %q", k.SystemPromptPrefix)
	}
	k.Step = &saveStep{}
	sess.kernel, sess.model = k, model

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	body := strings.NewReader(`{"session":"f1","model":"` + model + `","message":"save a doc with a beach packing list"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/chat", body)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var tools []toolEvent
	var names []string
	for _, e := range parseSSE(t, string(raw)) {
		names = append(names, e.name)
		switch e.name {
		case "tool":
			var te toolEvent
			_ = json.Unmarshal([]byte(e.data), &te)
			tools = append(tools, te)
		case "error":
			t.Fatalf("error event: %s", e.data)
		}
	}
	if got := strings.Join(names, ","); got != "session,prompt,activity,activity,tool,tool,activity,delta,done" {
		t.Fatalf("events = %s\n%s", got, raw)
	}
	const summary = "✓ saved: Beach packing.html (2 KB) · Documents/Toroid"
	want := []toolEvent{
		{ID: "call_1", Name: "file_save", State: "start", Text: "… saving a file"},
		{ID: "call_1", Name: "file_save", State: "done", Text: summary},
	}
	if !reflect.DeepEqual(tools, want) {
		t.Fatalf("tool events = %+v, want %+v", tools, want)
	}
	if saved.Name != "Beach packing.html" || saved.Content != savedDoc {
		t.Fatalf("host got %+v", saved)
	}

	rec, err := srv.chats.load("f1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Messages) != 2 {
		t.Fatalf("saved %d messages", len(rec.Messages))
	}
	m := rec.Messages[1]
	if m.Role != "assistant" || !reflect.DeepEqual(m.Tools, []string{summary}) || m.Text != "Saved Beach packing.html in Documents › Toroid." {
		t.Fatalf("saved reply = %+v", m)
	}
}

// The stable prefix must not change between requests, or the prompt cache
// misses on every message.
func TestSystemPromptPrefixIsStable(t *testing.T) {
	srv := newServer(t.TempDir(), "tok", 0.5)
	srv.tools = newToolHost(filepath.Join(t.TempDir(), "none"))
	var prefixes []string
	for i := 0; i < 2; i++ {
		k, err := srv.newKernelWithKey(context.Background(), defaultModels[0].ID, nil, 0.5, "test-key")
		if err != nil {
			t.Fatal(err)
		}
		prefixes = append(prefixes, k.SystemPromptPrefix)
		_ = k.Close()
	}
	if prefixes[0] != prefixes[1] || prefixes[0] != chatSystemPrompt {
		t.Fatal("system prompt prefix differs between kernels")
	}
	for _, rule := range []string{"explicitly asks to store or save", "Never save on your own initiative", "Delete a file only when the user explicitly asks", "task_add"} {
		if !strings.Contains(chatSystemPrompt, rule) {
			t.Errorf("prompt lacks %q", rule)
		}
	}
}
