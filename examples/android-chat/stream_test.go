package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

type sseEvent struct {
	name string
	data string
}

// parseSSE splits a raw SSE body into events, skipping comment lines.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if ev.data != "" {
					t.Fatalf("event %q has more than one data line: %q", ev.name, block)
				}
				ev.data = strings.TrimPrefix(line, "data: ")
			default:
				t.Fatalf("unexpected SSE line %q", line)
			}
		}
		if ev.name != "" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSSEFramingKeepsMultilineTextOnOneDataLine(t *testing.T) {
	var b strings.Builder
	s := newSSEWriter(&b)
	text := "line one\n\nline two\r\n```go\nfmt.Println(\"x\")\n```"
	if err := s.event("delta", textEvent{Text: text}); err != nil {
		t.Fatal(err)
	}
	if err := s.comment("ping"); err != nil {
		t.Fatal(err)
	}
	evs := parseSSE(t, b.String())
	if len(evs) != 1 || evs[0].name != "delta" {
		t.Fatalf("events = %+v, want one delta", evs)
	}
	var got textEvent
	if err := json.Unmarshal([]byte(evs[0].data), &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != text {
		t.Fatalf("round-trip text = %q, want %q", got.Text, text)
	}
}

func TestSSEFlushesEachEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEWriter(rec)
	if err := s.event("delta", textEvent{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed {
		t.Fatal("event was not flushed")
	}
}

func TestChatStreamEventsPerWrite(t *testing.T) {
	var b strings.Builder
	cs := &chatStream{sse: newSSEWriter(&b)}
	if _, err := io.WriteString(cs.reasoning(), "hmm"); err != nil {
		t.Fatal(err)
	}
	if cs.sentText() {
		t.Fatal("reasoning counted as reply text")
	}
	for _, s := range []string{"Hel", "", "lo"} {
		if _, err := io.WriteString(cs, s); err != nil {
			t.Fatal(err)
		}
	}
	evs := parseSSE(t, b.String())
	var names []string
	for _, e := range evs {
		names = append(names, e.name)
	}
	if got := strings.Join(names, ","); got != "reasoning,delta,delta" {
		t.Fatalf("events = %s, want reasoning,delta,delta (empty writes send nothing)", got)
	}
	if !cs.sentText() || cs.firstTokenAt().IsZero() {
		t.Fatal("text and first-token time not recorded")
	}
}

// scriptedStep streams fixed chunks and counts which method served the call.
type scriptedStep struct {
	reasoning string
	chunks    []string
	completes int
	streams   int
}

// msg is the assembled message; its usage carries a gateway-reported cost, as
// OpenRouter's usage.cost does.
func (s *scriptedStep) msg() *toroid.AssistantMessage {
	var content []llm.Part
	if s.reasoning != "" {
		content = append(content, llm.ReasoningPart{Text: s.reasoning})
	}
	content = append(content, llm.TextPart{Text: strings.Join(s.chunks, "")})
	return &toroid.AssistantMessage{
		Content:    content,
		Usage:      toroid.Usage{Input: 1000, Output: 500, Cost: 0.0002, PricingOK: true},
		StopReason: toroid.StopReasonStop,
	}
}

func (s *scriptedStep) Complete(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.AssistantMessage, error) {
	s.completes++
	return s.msg(), nil
}

func (s *scriptedStep) Stream(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.StreamResult, error) {
	s.streams++
	ch := make(chan toroid.StreamDelta, len(s.chunks)+1)
	if s.reasoning != "" {
		ch <- toroid.StreamDelta{Reasoning: s.reasoning}
	}
	for _, c := range s.chunks {
		ch <- toroid.StreamDelta{Text: c}
	}
	close(ch)
	return &toroid.StreamResult{Deltas: ch, Final: func() (*toroid.AssistantMessage, error) { return s.msg(), nil }}, nil
}

func (s *scriptedStep) CompleteObject(context.Context, toroid.Model, toroid.Context, toroid.Schema, string, string, toroid.StepOptions) (*toroid.ObjectResult, error) {
	return nil, nil
}

// TestChatEndpointStreamsKernelReply drives /api/chat end to end with a
// scripted Step in place of the network. The kernel's token streaming must
// carry each chunk to the client as its own event, through Step.Stream only.
func TestChatEndpointStreamsKernelReply(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	t.Setenv(toroid.GatewayBaseURLEnv, "http://127.0.0.1:1") // nothing listens; limits lookup fails fast
	srv := newServer(dir, "tok", 0.5)
	inner := &scriptedStep{reasoning: "greet", chunks: []string{"Hi", " there"}}

	model := defaultModels[0].ID
	sess := srv.sessions.get("s1")
	k, err := srv.newKernelWithKey(context.Background(), model, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	k.Step = inner
	sess.kernel, sess.model = k, model

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	body := strings.NewReader(`{"session":"s1","model":"` + model + `","message":"hello"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/chat", body)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	raw, _ := io.ReadAll(bufio.NewReader(resp.Body))
	evs := parseSSE(t, string(raw))
	var names []string
	var text strings.Builder
	var done doneEvent
	for _, e := range evs {
		names = append(names, e.name)
		switch e.name {
		case "delta":
			var d textEvent
			_ = json.Unmarshal([]byte(e.data), &d)
			text.WriteString(d.Text)
		case "done":
			_ = json.Unmarshal([]byte(e.data), &done)
		}
	}
	if got := strings.Join(names, ","); got != "session,prompt,activity,reasoning,delta,delta,done" {
		t.Fatalf("events = %s\n%s", got, raw)
	}
	if text.String() != "Hi there" {
		t.Fatalf("text = %q", text.String())
	}
	if inner.streams != 1 || inner.completes != 0 {
		t.Fatalf("step served streams=%d completes=%d; want the streaming path only", inner.streams, inner.completes)
	}
	if done.ReplyCostUSD != 0.0002 || done.SessionCostUSD != done.ReplyCostUSD {
		t.Fatalf("done = %+v, want the gateway-reported cost", done)
	}
}

func TestAPIRequiresTokenAndLoopbackHost(t *testing.T) {
	srv := newServer(t.TempDir(), "tok", 0.5)
	h := srv.routes()
	cases := []struct {
		name, host, auth string
		want             int
	}{
		{"no token", "127.0.0.1:18765", "", http.StatusUnauthorized},
		{"bad token", "127.0.0.1:18765", "Bearer nope", http.StatusUnauthorized},
		{"ok", "127.0.0.1:18765", "Bearer tok", http.StatusOK},
		{"localhost", "localhost:18765", "Bearer tok", http.StatusOK},
		{"rebound host", "evil.example:18765", "Bearer tok", http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
		req.Host = c.host
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestStatusNeverEchoesKey(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(dir, "tok", 0.5)
	h := srv.routes()
	const secret = "sk-or-v1-secretvalue1234"
	req := httptest.NewRequest(http.MethodPost, "/api/key", strings.NewReader(`{"key":"`+secret+`"}`))
	req.Host = "127.0.0.1:18765"
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save key: %d %s", rec.Code, rec.Body)
	}
	if k, src := srv.readKey(); k != secret || src != "file" {
		t.Fatalf("readKey = (len %d, %q)", len(k), src)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "127.0.0.1:18765"
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "1234") || !strings.Contains(rec.Body.String(), `"has_key":true`) {
		t.Fatalf("status body %s", rec.Body)
	}
}
