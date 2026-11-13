package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseWriter frames Server-Sent Events: "event: <name>\ndata: <json>\n\n",
// flushed one event at a time so each delta reaches the WebView as soon as it
// is produced. JSON-encoding the data keeps every event on one data line, since
// json.Marshal escapes the newlines inside model text.
type sseWriter struct {
	mu sync.Mutex
	w  io.Writer
	f  http.Flusher // nil when the writer cannot flush (tests)
}

func newSSEWriter(w io.Writer) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

// event writes one event. Safe for concurrent use: kernel hooks may fire from
// other goroutines than the request handler.
func (s *sseWriter) event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.raw(name, b)
}

// raw writes one event whose data is already JSON.
func (s *sseWriter) raw(name string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

// comment writes an SSE comment line, used as a keep-alive that clients ignore.
func (s *sseWriter) comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

// textEvent is the data of "delta" and "reasoning" events.
type textEvent struct {
	Text string `json:"text"`
}

// toolEvent is the data of a "tool" event: State "start" when a call begins,
// then "done" or "error" with the call's one-line summary in Text.
type toolEvent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Text  string `json:"text"`
}

// chatStream bridges one Kernel.Stream call to one SSE response. With
// toroid.WithTokenStream the kernel writes reply text to it as the model
// generates it, and with toroid.WithReasoningStream it writes reasoning to
// reasoning(); each write becomes one SSE event. Tool calls reach it through
// the kernel's hooks (see streamFrom) and become "tool" events.
type chatStream struct {
	sse eventSink // the turn, which records events for its followers

	mu         sync.Mutex
	text       strings.Builder // reply text sent to the client
	firstToken time.Time       // when the first reply or reasoning token arrived
	tools      []string        // one summary per finished tool call, in order
	breakNext  bool            // a tool ran since the last text: start a new paragraph
	displays   map[int]bool    // displays already announced this turn
}

// note records text about to be sent and returns it as sent: text written
// after a tool call starts a new paragraph, since the kernel writes each
// turn's text with no separator.
func (c *chatStream) note(text []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.firstToken.IsZero() {
		c.firstToken = time.Now()
	}
	if len(text) == 0 {
		return text
	}
	if c.breakNext && c.text.Len() > 0 {
		text = append([]byte("\n\n"), text...)
	}
	c.breakNext = false
	c.text.Write(text)
	return text
}

// Write receives reply text from the kernel.
func (c *chatStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	sent := c.note(p)
	if err := c.sse.event("delta", textEvent{Text: string(sent)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// displayEvent is the data of a "display" event: a background display became
// active this turn, so the UI shows a small live-video panel for it.
type displayEvent struct {
	ID     int `json:"id"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// displayActive tells the client a background display is in use this turn, so
// it can show the live view. Each display is announced once per turn.
func (c *chatStream) displayActive(id, w, h int) {
	c.mu.Lock()
	seen := c.displays[id]
	if c.displays == nil {
		c.displays = map[int]bool{}
	}
	c.displays[id] = true
	c.mu.Unlock()
	if !seen {
		_ = c.sse.event("display", displayEvent{ID: id, Width: w, Height: h})
	}
}

// activityEvent is the data of an "activity" event: who the reply is waiting
// on now. Kind is "llm" (the chat model is deciding the next step), "vision"
// (a screenshot from Tool went to the vision model) or "tool" (Tool is
// running on the phone). Model is a gateway model id.
type activityEvent struct {
	Kind  string `json:"kind"`
	Model string `json:"model,omitempty"`
	Tool  string `json:"tool,omitempty"`
}

func (c *chatStream) activity(kind, model, tool string) {
	_ = c.sse.event("activity", activityEvent{Kind: kind, Model: model, Tool: tool})
}

// toolStarted reports a tool call the model just made.
func (c *chatStream) toolStarted(id, name string) {
	c.activity("tool", "", name)
	c.mu.Lock()
	c.breakNext = true
	c.mu.Unlock()
	_ = c.sse.event("tool", toolEvent{ID: id, Name: name, State: "start", Text: toolStartText(name)})
}

// toolFinished reports a call's outcome and keeps its summary for the saved
// chat. errText is set when the call failed.
func (c *chatStream) toolFinished(id, name, result, errText string) {
	line := toolSummary(name, result, errText)
	c.mu.Lock()
	c.tools = append(c.tools, line)
	c.mu.Unlock()
	state := "done"
	if errText != "" {
		state = "error"
	}
	_ = c.sse.event("tool", toolEvent{ID: id, Name: name, State: state, Text: line})
}

// toolLines is the summary of every tool call so far.
func (c *chatStream) toolLines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.tools...)
}

// Kernel hooks are registered once per kernel, but each reply has its own
// chatStream. The kernel hands hooks the context of the Stream call that fired
// them, so the reply's chatStream rides in that context: a hook always reaches
// the stream of the reply it belongs to, with no shared field to swap between
// requests, and an event outside any reply finds none.
type streamKey struct{}

func withChatStream(ctx context.Context, c *chatStream) context.Context {
	return context.WithValue(ctx, streamKey{}, c)
}

// streamFrom returns the chatStream of the reply ctx belongs to, or nil.
func streamFrom(ctx context.Context) *chatStream {
	c, _ := ctx.Value(streamKey{}).(*chatStream)
	return c
}

// reasoning returns the writer for the model's reasoning text.
func (c *chatStream) reasoning() io.Writer { return reasoningWriter{c} }

type reasoningWriter struct{ c *chatStream }

func (r reasoningWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.c.note(nil)
	if err := r.c.sse.event("reasoning", textEvent{Text: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// firstTokenAt reports when the first token arrived (zero if none did).
func (c *chatStream) firstTokenAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstToken
}

// sentText reports whether any reply text reached the client.
func (c *chatStream) sentText() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.Len() > 0
}

// replyText is the reply text the client received, which is what gets saved.
func (c *chatStream) replyText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}
