package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

// testRecord is a two-turn chat started at at.
func testRecord(id string, at time.Time) *chatRecord {
	rec := &chatRecord{ID: id, Model: defaultModels[0].ID, CostUSD: 0.001}
	rec.add(chatMessage{Role: "user", Text: "what is " + id + "?", At: stamp(at)})
	rec.add(chatMessage{Role: "assistant", Text: "it is " + id, Model: rec.Model, At: stamp(at.Add(time.Second)), CostUSD: 0.001})
	return rec
}

func TestChatStoreRoundTrip(t *testing.T) {
	st := newChatStore(filepath.Join(t.TempDir(), "chats"))
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.Local)
	rec := testRecord("abc123", at)
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	got, err := st.load("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID || got.Title != "what is abc123?" || got.CreatedAt != stamp(at) ||
		got.UpdatedAt != stamp(at.Add(time.Second)) || got.Model != rec.Model || got.CostUSD != rec.CostUSD {
		t.Fatalf("loaded %+v, want %+v", got, rec)
	}
	if len(got.Messages) != 2 || !reflect.DeepEqual(got.Messages[1], rec.Messages[1]) {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if _, err := st.load("missing"); !os.IsNotExist(err) {
		t.Fatalf("load(missing) err = %v, want not-exist", err)
	}
}

func TestChatStoreWritesAtomicallyWithPrivateModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chats")
	st := newChatStore(dir)
	if err := st.save(&chatRecord{ID: "empty"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a chat with no messages was written (stat err %v)", err)
	}
	rec := testRecord("c1", time.Now())
	for i := 0; i < 2; i++ { // the second save replaces the first
		if err := st.save(rec); err != nil {
			t.Fatal(err)
		}
		rec.add(chatMessage{Role: "user", Text: "more", At: stamp(time.Now())})
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(names) != 1 || filepath.Base(names[0]) != "c1.json" {
		t.Fatalf("files = %v, want only c1.json (no temp file left)", names)
	}
	fi, _ := os.Stat(names[0])
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file=%v dir=%v, want 0600 and 0700", fi.Mode().Perm(), di.Mode().Perm())
	}
	got, _ := st.load("c1")
	if len(got.Messages) != 3 {
		t.Fatalf("saved %d messages, want 3", len(got.Messages))
	}
}

func TestChatTitle(t *testing.T) {
	if got := chatTitle("  hello\n\n  world  "); got != "hello world" {
		t.Fatalf("title = %q", got)
	}
	long := strings.Repeat("é", 70)
	got := chatTitle(long)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != maxTitleRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long title = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
	if got := chatTitle(strings.Repeat("a", maxTitleRunes)); got != strings.Repeat("a", maxTitleRunes) {
		t.Fatalf("a title of exactly %d runes was cut: %q", maxTitleRunes, got)
	}
}

func TestHistoryNewestFirst(t *testing.T) {
	st := newChatStore(filepath.Join(t.TempDir(), "chats"))
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"b", "c", "a"} {
		if err := st.save(testRecord(id, base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	sums, err := st.list()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range sums {
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, ","); got != "a,c,b" {
		t.Fatalf("order = %s, want newest first: a,c,b", got)
	}
	if sums[0].MessageCount != 2 || sums[0].Title != "what is a?" {
		t.Fatalf("summary = %+v", sums[0])
	}
	// Files removed or added behind the store's back show up on the next list.
	if err := st.remove("c"); err != nil {
		t.Fatal(err)
	}
	if sums, _ := st.list(); len(sums) != 2 {
		t.Fatalf("after remove, %d chats", len(sums))
	}
}

// TestHistoryDayIsLocalDate buckets by the local calendar date: 20:00 UTC is
// already the next day in Nepal (UTC+05:45).
func TestHistoryDayIsLocalDate(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	old := localZone
	localZone = func() *time.Location { return loc }
	defer func() { localZone = old }()

	st := newChatStore(filepath.Join(t.TempDir(), "chats"))
	if err := st.save(testRecord("late", time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	sums, _ := st.list()
	if len(sums) != 1 || sums[0].Day != "2026-11-12" || !strings.HasSuffix(sums[0].CreatedAt, "+05:45") {
		t.Fatalf("summary = %+v, want day 2026-11-12 stamped +05:45", sums)
	}
}

func TestChatIDValidation(t *testing.T) {
	for id, ok := range map[string]bool{
		"18f3a2b4c5d6e7f8":      true,
		"zz-test-1_a":           true,
		"":                      false,
		"..":                    false,
		"../openrouter.key":     false,
		"a/b":                   false,
		"a.json":                false,
		strings.Repeat("a", 65): false,
	} {
		if validChatID(id) != ok {
			t.Errorf("validChatID(%q) = %v, want %v", id, !ok, ok)
		}
	}
	st := newChatStore(t.TempDir())
	if _, err := st.load("../openrouter.key"); err == nil || os.IsNotExist(err) {
		t.Fatalf("load of a traversal id: err = %v, want a bad-id error", err)
	}
}

func TestChatEndpointsRefuseBadIDs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.json"), []byte(`{"id":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newServer(dir, "tok", 0.5)
	h := srv.routes()
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/chats/..%2Fsecret", http.StatusBadRequest},
		{http.MethodGet, "/api/chats/a.b", http.StatusBadRequest},
		{http.MethodDelete, "/api/chats/..%2Fsecret", http.StatusBadRequest},
		{http.MethodGet, "/api/chats/nochat", http.StatusNotFound},
		{http.MethodDelete, "/api/chats/nochat", http.StatusNotFound},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Host = "127.0.0.1:18765"
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	body := strings.NewReader(`{"session":"../x","model":"` + defaultModels[0].ID + `","message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	req.Host = "127.0.0.1:18765"
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("chat with a traversal session id: status %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.json")); err != nil {
		t.Fatalf("file outside chats/ touched: %v", err)
	}
}

// recordingStep is a scriptedStep that also keeps the messages it was sent.
type recordingStep struct {
	scriptedStep
	sent []llm.Message
}

func (s *recordingStep) Stream(ctx context.Context, m toroid.Model, c toroid.Context, o toroid.StepOptions) (*toroid.StreamResult, error) {
	s.sent = append([]llm.Message(nil), c.Messages...)
	return s.scriptedStep.Stream(ctx, m, c, o)
}

// postChat sends one message to /api/chat and returns the done event.
func postChat(t *testing.T, url, session, model, message string) doneEvent {
	t.Helper()
	b, _ := json.Marshal(chatRequest{Session: session, Model: model, Message: message})
	req, _ := http.NewRequest(http.MethodPost, url+"/api/chat", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var done doneEvent
	found := false
	for _, e := range parseSSE(t, string(raw)) {
		switch e.name {
		case "done":
			found = true
			_ = json.Unmarshal([]byte(e.data), &done)
		case "error":
			t.Fatalf("error event: %s", e.data)
		}
	}
	if !found {
		t.Fatalf("no done event:\n%s", raw)
	}
	return done
}

// TestChatIsSavedAndResumesAfterRestart saves a chat through /api/chat, then
// starts a second server on the same data dir (an app restart) and continues
// the chat: the new kernel must start from the saved turns, and the money spent
// before the restart must still count.
func TestChatIsSavedAndResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	t.Setenv(toroid.GatewayBaseURLEnv, "http://127.0.0.1:1")
	t.Setenv(toroid.GatewayKeyEnv, "test-key")
	model := defaultModels[0].ID

	// First run: a chat whose kernel replies through a scripted Step.
	srv := newServer(dir, "tok", 0.5)
	sess := srv.sessions.get("chat1")
	k, err := srv.newKernelWithKey(context.Background(), model, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	k.Step = &scriptedStep{chunks: []string{"Paris", "."}}
	sess.kernel, sess.model = k, model
	ts := httptest.NewServer(srv.routes())
	postChat(t, ts.URL, "chat1", model, "Capital of France?")
	ts.Close()
	srv.sessions.closeAll()

	rec, err := srv.chats.load("chat1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Title != "Capital of France?" || rec.Model != model || !near(rec.CostUSD, 0.0002) || len(rec.Messages) != 2 {
		t.Fatalf("saved record = %+v", rec)
	}
	if m := rec.Messages[1]; m.Role != "assistant" || m.Text != "Paris." || m.Model != model || !near(m.CostUSD, 0.0002) {
		t.Fatalf("saved reply = %+v", m)
	}
	if sums, _ := srv.chats.list(); len(sums) != 1 || sums[0].ID != "chat1" || sums[0].MessageCount != 2 {
		t.Fatalf("history = %+v", sums)
	}

	// Restart: nothing in memory. Load the chat the way /api/chat does, give
	// the rebuilt kernel a scripted Step, then send a follow-up.
	srv2 := newServer(dir, "tok", 0.5)
	sess2 := srv2.sessions.get("chat1")
	if err := srv2.loadRecord(sess2, "chat1"); err != nil {
		t.Fatal(err)
	}
	if err := srv2.ensureKernel(context.Background(), sess2, model, newSSEWriter(io.Discard)); err != nil {
		t.Fatal(err)
	}
	h := sess2.kernel.History
	if len(h) != 2 || h[0].Role != llm.RoleUser || h[0].Text() != "Capital of France?" ||
		h[1].Role != llm.RoleAssistant || h[1].Text() != "Paris." {
		t.Fatalf("rebuilt history = %+v", h)
	}
	step := &recordingStep{scriptedStep: scriptedStep{chunks: []string{"About 2 million."}}}
	sess2.kernel.Step = step
	ts2 := httptest.NewServer(srv2.routes())
	defer ts2.Close()
	done := postChat(t, ts2.URL, "chat1", model, "Population?")

	if len(step.sent) != 3 || step.sent[1].Text() != "Paris." || !strings.HasPrefix(step.sent[2].Text(), "<system-reminder>\n") ||
		!strings.HasSuffix(step.sent[2].Text(), "</system-reminder>\n\nPopulation?") {
		t.Fatalf("model was sent %d messages: %+v", len(step.sent), step.sent)
	}
	if !near(done.ReplyCostUSD, 0.0002) || !near(done.SessionCostUSD, 0.0004) {
		t.Fatalf("done = %+v, want the pre-restart spend carried over", done)
	}
	rec, _ = srv2.chats.load("chat1")
	if len(rec.Messages) != 4 || !near(rec.CostUSD, 0.0004) || rec.Title != "Capital of France?" {
		t.Fatalf("record after resume = %+v", rec)
	}
}

func TestChatHistoryMergesUnansweredTurns(t *testing.T) {
	rec := &chatRecord{ID: "x"}
	now := stamp(time.Now())
	rec.add(chatMessage{Role: "user", Text: "first", At: now})
	rec.add(chatMessage{Role: "user", Text: "again", At: now})
	rec.add(chatMessage{Role: "assistant", Text: "hi", At: now})
	h := rec.history()
	if len(h) != 2 || h[0].Text() != "first\n\nagain" || h[1].Role != llm.RoleAssistant {
		t.Fatalf("history = %+v", h)
	}
}

func TestDeleteChat(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(dir, "tok", 0.5)
	if err := srv.chats.save(testRecord("gone", time.Now())); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/chats/gone", nil)
	req.Host = "127.0.0.1:18765"
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if sums, _ := srv.chats.list(); len(sums) != 0 {
		t.Fatalf("history after delete = %+v", sums)
	}
}
