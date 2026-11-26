package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// A reply belongs to the chat. Each HTTP reader independently replays its frames
// from the transcript boundary and then waits for newly published frames.
type liveReply struct {
	id      string
	before  int64
	cancel  context.CancelFunc
	mu      sync.Mutex
	frames  [][]byte
	changed chan struct{}
	done    bool
}

func (a *app) live(chat string) *liveReply {
	value, _ := a.active.Load(chat)
	run, _ := value.(*liveReply)
	return run
}

func (s *liveReply) publish(frame any) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	s.appendLine(line, false)
	return nil
}

func (s *liveReply) finish(frame any) {
	line, _ := json.Marshal(frame)
	s.appendLine(line, true)
}

func (s *liveReply) appendLine(line []byte, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, append(line, '\n'))
	s.done = done
	close(s.changed)
	if !done {
		s.changed = make(chan struct{})
	}
}

func (s *liveReply) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	cursor := 0
	for {
		s.mu.Lock()
		batch, changed, done := s.frames[cursor:], s.changed, s.done
		s.mu.Unlock()
		if len(batch) > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			for _, line := range batch {
				if _, err := w.Write(line); err != nil {
					return
				}
			}
			if err := rc.Flush(); err != nil {
				return
			}
			_ = rc.SetWriteDeadline(time.Time{})
			cursor += len(batch)
		}
		if done {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		}
	}
}

func (a *app) follow(w http.ResponseWriter, r *http.Request) {
	run := a.live(r.PathValue("id"))
	if run == nil || run.id != r.PathValue("run") {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	run.serve(w, r)
}

func (a *app) stop(w http.ResponseWriter, r *http.Request) {
	if run := a.live(r.PathValue("id")); run != nil && run.id == r.PathValue("run") {
		run.cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) seen(w http.ResponseWriter, r *http.Request) {
	var req struct{ Ready int64 }
	if !decode(w, r, &req) {
		return
	}
	_, err := a.db.ExecContext(r.Context(), `UPDATE webui_replies SET seen=? WHERE trace_id=? AND ready>=? AND seen<?`, req.Ready, r.PathValue("id"), req.Ready, req.Ready)
	if err != nil {
		dbError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
