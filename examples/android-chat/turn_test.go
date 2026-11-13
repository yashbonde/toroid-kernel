package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

// gatedStep streams "Hi", then waits for gate (or the turn's context) before
// streaming " there", so a test can act while a reply is in flight.
type gatedStep struct {
	scriptedStep
	gate chan struct{}
}

func (g *gatedStep) Stream(ctx context.Context, _ toroid.Model, _ toroid.Context, _ toroid.StepOptions) (*toroid.StreamResult, error) {
	ch := make(chan toroid.StreamDelta)
	errc := make(chan error, 1)
	go func() {
		defer close(ch)
		ch <- toroid.StreamDelta{Text: "Hi"}
		select {
		case <-g.gate:
			ch <- toroid.StreamDelta{Text: " there"}
			errc <- nil
		case <-ctx.Done():
			errc <- ctx.Err()
		}
	}()
	return &toroid.StreamResult{Deltas: ch, Final: func() (*toroid.AssistantMessage, error) {
		if err := <-errc; err != nil {
			return nil, err
		}
		return g.msg(), nil
	}}, nil
}

// turnServer is a server whose chat "s1" answers through a gatedStep.
func turnServer(t *testing.T) (*server, *httptest.Server, chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	srv := newServer(dir, "tok", 0.5)
	gate := make(chan struct{})
	model := defaultModels[0].ID
	sess := srv.sessions.get("s1")
	k, err := srv.newKernelWithKey(context.Background(), model, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	k.Step = &gatedStep{scriptedStep: scriptedStep{chunks: []string{"Hi", " there"}}, gate: gate}
	sess.kernel, sess.model = k, model
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return srv, ts, gate
}

func authed(t *testing.T, ctx context.Context, method, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readUntil reads SSE lines until one names the event want.
func readUntil(t *testing.T, r *bufio.Reader, want string) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before %q: %v", want, err)
		}
		if strings.TrimSpace(line) == "event: "+want {
			return
		}
	}
}

func waitIdle(t *testing.T, srv *server, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for srv.liveTurn(id) != nil {
		if time.Now().After(deadline) {
			t.Fatal("reply still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startChat(t *testing.T, ctx context.Context, ts *httptest.Server) *http.Response {
	t.Helper()
	model := defaultModels[0].ID
	return authed(t, ctx, http.MethodPost, ts.URL+"/api/chat", `{"session":"s1","model":"`+model+`","message":"hello"}`)
}

// The app going to the background drops the stream; the reply must carry on
// and be saved in full.
func TestTurnOutlivesItsClient(t *testing.T) {
	srv, ts, gate := turnServer(t)
	ctx, drop := context.WithCancel(context.Background())
	resp := startChat(t, ctx, ts)
	readUntil(t, bufio.NewReader(resp.Body), "delta") // "Hi" is out
	drop()
	resp.Body.Close()

	close(gate)
	waitIdle(t, srv, "s1")
	rec, err := srv.chats.load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Messages); n != 2 || rec.Messages[1].Text != "Hi there" {
		t.Fatalf("saved %+v", rec.Messages)
	}
}

// A client that comes back follows the running reply from its first event.
func TestLiveReplaysThenFollows(t *testing.T) {
	srv, ts, gate := turnServer(t)
	first := startChat(t, context.Background(), ts)
	defer first.Body.Close()
	readUntil(t, bufio.NewReader(first.Body), "delta")

	live := authed(t, context.Background(), http.MethodGet, ts.URL+"/api/chats/s1/live", "")
	defer live.Body.Close()
	if live.StatusCode != http.StatusOK {
		t.Fatalf("live: %d", live.StatusCode)
	}
	close(gate)
	raw, _ := io.ReadAll(live.Body)
	var names []string
	for _, e := range parseSSE(t, string(raw)) {
		names = append(names, e.name)
	}
	got := strings.Join(names, ",")
	if !strings.HasPrefix(got, "session,prompt,") || !strings.HasSuffix(got, ",done") || strings.Count(got, "delta") != 2 {
		t.Fatalf("live events = %s", got)
	}

	waitIdle(t, srv, "s1")
	idle := authed(t, context.Background(), http.MethodGet, ts.URL+"/api/chats/s1/live", "")
	idle.Body.Close()
	if idle.StatusCode != http.StatusNoContent {
		t.Fatalf("live after the reply: %d", idle.StatusCode)
	}
}

// Stop ends the reply; the stream says so and the message is still saved.
func TestStopEndsTheReply(t *testing.T) {
	srv, ts, _ := turnServer(t)
	resp := startChat(t, context.Background(), ts)
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	readUntil(t, r, "delta")

	stop := authed(t, context.Background(), http.MethodPost, ts.URL+"/api/chats/s1/stop", "")
	stop.Body.Close()
	readUntil(t, r, "stopped")
	waitIdle(t, srv, "s1")
	rec, err := srv.chats.load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Messages[0].Text != "hello" {
		t.Fatalf("saved %+v", rec.Messages)
	}
}

// toolLoopStep answers every step with "Let me look again." and an app_list
// call, so a reply only ends at the kernel's step cap.
type toolLoopStep struct{ scriptedStep }

func (s *toolLoopStep) msg(n int) *toroid.AssistantMessage {
	return &toroid.AssistantMessage{
		Content: []llm.Part{
			llm.TextPart{Text: "Let me look again."},
			llm.ToolCallPart{ID: fmt.Sprintf("c%d", n), Name: "app_list", Arguments: "{}"},
		},
		Usage:      toroid.Usage{Input: 100, Output: 10, Cost: 0.00001, PricingOK: true},
		StopReason: toroid.StopReasonToolUse,
	}
}

func (s *toolLoopStep) Stream(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.StreamResult, error) {
	s.streams++
	m := s.msg(s.streams)
	ch := make(chan toroid.StreamDelta, 1)
	ch <- toroid.StreamDelta{Text: "Let me look again."}
	close(ch)
	return &toroid.StreamResult{Deltas: ch, Final: func() (*toroid.AssistantMessage, error) { return m, nil }}, nil
}

// TestStepCapSaysItStopped checks that a reply ended by the kernel's step cap
// tells the user it stopped unfinished, in the stream and in the saved chat,
// instead of ending on the model's last "Let me look again."
func TestStepCapSaysItStopped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	t.Setenv(toroid.GatewayBaseURLEnv, "http://127.0.0.1:1")
	srv := newServer(dir, "tok", 0.5)
	srv.apps = newAppsBridge("@toroid-test-no-bridge-" + fmt.Sprint(time.Now().UnixNano()))
	model := defaultModels[0].ID
	k, err := srv.newKernelWithKey(context.Background(), model, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	step := &toolLoopStep{}
	k.Step = step
	k.Cfg.MaxIter = 2
	sess := srv.sessions.get("s1")
	sess.kernel, sess.model = k, model

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/chat", strings.NewReader(`{"session":"s1","model":"`+model+`","message":"hn top 5"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var text strings.Builder
	for _, e := range parseSSE(t, string(raw)) {
		if e.name == "delta" {
			var d textEvent
			_ = json.Unmarshal([]byte(e.data), &d)
			text.WriteString(d.Text)
		}
	}
	if step.streams != 2 {
		t.Fatalf("steps = %d, want the cap of 2", step.streams)
	}
	if !strings.HasSuffix(text.String(), "\n\n"+guardNote("max_iter")) {
		t.Fatalf("reply = %q, want it to end with the step-cap note", text.String())
	}
	rec, err := srv.chats.load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if last := rec.Messages[len(rec.Messages)-1]; !strings.Contains(last.Text, guardNote("max_iter")) {
		t.Fatalf("saved reply = %q, want the step-cap note", last.Text)
	}
}

// The reminder rides on a chat's first message and on the first after a
// quiet gap of more than reminderGap; messages in between go without one.
func TestReminderOnlyAfterAGap(t *testing.T) {
	var c chatSession
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		at   time.Duration
		want bool
	}{
		{0, true},                          // first message
		{time.Hour, false},                 // an hour later
		{7 * time.Hour, false},             // 6h after the previous message, not more
		{13*time.Hour + time.Minute, true}, // 6h01m quiet
		{13*time.Hour + 2*time.Minute, false},
	} {
		if got := c.needsReminder(t0.Add(step.at)); got != step.want {
			t.Fatalf("at +%v: reminder = %v, want %v", step.at, got, step.want)
		}
	}
}
