package toroid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// SessionDir returns ~/.toroid/sessions/<sessionID>, creating it if needed. It
// is the per-session directory that also holds the tool-output/ overflow files
// (see tools.TruncateToolOutput) and the transcript.jsonl event log.
func SessionDir(sessionID string) (string, error) {
	home, err := toroidHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "sessions", sessionID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// transcriptRecord is one OTEL span-event line in transcript.jsonl. It pairs
// spec-valid OpenTelemetry trace/span IDs with the event's canonical OTEL
// projection (name, timestamp, JSON payload attribute) — the same Event.OTEL()
// mapping used by OTELSpans and the Langfuse exporter, so the on-disk log can
// never drift from the exported trace.
type transcriptRecord struct {
	TraceID      string          `json:"trace_id"`
	SpanID       string          `json:"span_id"`
	ParentSpanID string          `json:"parent_span_id,omitempty"`
	Seq          uint64          `json:"seq"`
	Name         string          `json:"name"`
	TimeUnixNano int64           `json:"time_unix_nano"`
	Attribute    json.RawMessage `json:"attribute,omitempty"`
}

// transcriptWriter appends every observable event of a session to
// ~/.toroid/sessions/<session-id>/transcript.jsonl as newline-delimited,
// OTEL-shaped records. It is always-on — independent of Save/SQLite — and
// best-effort: a write failure never disrupts the run. Writes are serialized so
// events firing from concurrent goroutines each land on their own intact line.
type transcriptWriter struct {
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
	Path string
}

// Close closes the underlying file. Safe on a nil writer.
func (w *transcriptWriter) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	return w.f.Close()
}
