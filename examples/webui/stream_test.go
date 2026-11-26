package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReplyCanBeReplayedAfterReaderDisconnects(t *testing.T) {
	run := &liveReply{changed: make(chan struct{})}
	emit := func(text string) {
		t.Helper()
		if err := run.publish(map[string]string{"text": text}); err != nil {
			t.Fatal(err)
		}
	}
	emit("first")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	run.serve(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "GET", "/", nil))
	emit("second")
	run.finish(map[string]string{"type": "done"})
	for range 2 {
		w := httptest.NewRecorder()
		run.serve(w, httptest.NewRequest("GET", "/", nil))
		d := json.NewDecoder(strings.NewReader(w.Body.String()))
		for _, want := range []string{"first", "second", "done"} {
			var frame map[string]string
			if err := d.Decode(&frame); err != nil || frame["text"]+frame["type"] != want {
				t.Fatalf("got %v, %v; want %s", frame, err, want)
			}
		}
	}
}
