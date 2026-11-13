package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// A turn is one reply being generated. It runs on its own context, not the
// request's, so it keeps going when the app goes to the background and the
// WebView's stream drops; only Stop (POST /api/chats/{id}/stop) or the spend
// cap ends it early. Every event is kept, so a client that comes back can
// replay the turn from its first event and then follow it live
// (GET /api/chats/{id}/live).

// eventSink takes the events of a reply: a turn records them; tests may use
// an sseWriter directly.
type eventSink interface {
	event(name string, data any) error
}

type turnEvent struct {
	name string
	data []byte
}

type turn struct {
	cancel context.CancelFunc

	mu     sync.Mutex
	events []turnEvent
	subs   map[chan struct{}]struct{} // followers, woken on each new event and at the end
	ended  bool
}

func newTurn(cancel context.CancelFunc) *turn {
	return &turn{cancel: cancel, subs: map[chan struct{}]struct{}{}}
}

// event records one event and wakes the followers. It never fails on a slow
// or gone client: followers write on their own goroutines.
func (t *turn) event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.events = append(t.events, turnEvent{name, b})
	t.wakeLocked()
	t.mu.Unlock()
	return nil
}

// finish marks the turn over; followers write what is left and return.
func (t *turn) finish() {
	t.mu.Lock()
	t.ended = true
	t.wakeLocked()
	t.mu.Unlock()
}

func (t *turn) wakeLocked() {
	for ch := range t.subs {
		select {
		case ch <- struct{}{}:
		default: // already has a wake-up pending
		}
	}
}

// follow writes the turn's events to w from the first one, then each new one
// as it comes, until the turn ends or ctx (the client's request) is done.
// Keep-alives go out while the turn is quiet, e.g. while a reasoning model
// thinks before its first visible token.
func (t *turn) follow(ctx context.Context, w *sseWriter) {
	wake := make(chan struct{}, 1)
	t.mu.Lock()
	t.subs[wake] = struct{}{}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.subs, wake)
		t.mu.Unlock()
	}()

	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	next := 0
	for {
		t.mu.Lock()
		pending, ended := t.events[next:], t.ended
		t.mu.Unlock()
		for _, e := range pending {
			if err := w.raw(e.name, e.data); err != nil {
				return
			}
		}
		next += len(pending)
		if ended {
			return
		}
		select {
		case <-wake:
		case <-ping.C:
			if w.comment("ping") != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// startSSE sends the headers of an event stream.
func startSSE(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return newSSEWriter(w)
}

// handleLive follows the reply running in a chat, replaying it from the
// start: the UI calls it when it comes back from the background, or opens a
// chat that is still answering. 204 when nothing is running.
func (s *server) handleLive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validChatID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad chat id"})
		return
	}
	t := s.liveTurn(id)
	if t == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	t.follow(r.Context(), startSSE(w))
}

// handleStop ends the reply running in a chat, as the Stop button. The turn
// then saves what it had and ends its stream with a "stopped" event.
func (s *server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validChatID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad chat id"})
		return
	}
	t := s.liveTurn(id)
	if t != nil {
		t.cancel()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopped": t != nil})
}

// liveTurn is the reply running in chat id, or nil.
func (s *server) liveTurn(id string) *turn {
	sess, ok := s.sessions.lookup(id)
	if !ok {
		return nil
	}
	return sess.liveTurn()
}
