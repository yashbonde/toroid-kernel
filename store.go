package toroid

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
	_ "modernc.org/sqlite"
)

// Persistence is consolidated on a single embedded SQLite database
// (~/.toroid/sql.db) holding traces, spans, costs, events, memories — and
// the todo table created by the tools package. The trace/span/parent IDs map
// directly onto OpenTelemetry (see id.go / §12.2).

// TraceMeta is stored per trace (root kernel run).
type TraceMeta struct {
	TraceID         string `json:"trace_id"`
	Title           string `json:"title,omitempty"`
	StartedAt       int64  `json:"started_at"` // UnixNano
	EndedAt         int64  `json:"ended_at,omitempty"`
	PreviousTraceID string `json:"previous_trace_id,omitempty"`
}

// SpanMeta is stored per span (kernel session, including subagents).
type SpanMeta struct {
	SpanID       string `json:"span_id"`
	TraceID      string `json:"trace_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	Model        string `json:"model,omitempty"`
	Title        string `json:"title,omitempty"`
	StartedAt    int64  `json:"started_at"` // UnixNano
	EndedAt      int64  `json:"ended_at,omitempty"`
}

// Store wraps the shared SQLite database for all persistence needs.
type Store struct {
	db *sql.DB
}

var (
	dbMu      sync.Mutex
	defaultDB *sql.DB
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS traces (
	trace_id          TEXT PRIMARY KEY,
	title             TEXT    NOT NULL DEFAULT '',
	started_at        INTEGER NOT NULL DEFAULT 0,
	ended_at          INTEGER NOT NULL DEFAULT 0,
	previous_trace_id TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS spans (
	trace_id       TEXT    NOT NULL,
	span_id        TEXT    NOT NULL,
	parent_span_id TEXT    NOT NULL DEFAULT '',
	model          TEXT    NOT NULL DEFAULT '',
	title          TEXT    NOT NULL DEFAULT '',
	started_at     INTEGER NOT NULL DEFAULT 0,
	ended_at       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (trace_id, span_id)
);
CREATE TABLE IF NOT EXISTS costs (
	trace_id  TEXT    NOT NULL,
	span_id   TEXT    NOT NULL,
	ts        INTEGER NOT NULL,
	turn_usd  REAL    NOT NULL,
	total_usd REAL    NOT NULL
);
CREATE INDEX IF NOT EXISTS costs_idx ON costs (trace_id, span_id, ts);
CREATE TABLE IF NOT EXISTS events (
	trace_id TEXT    NOT NULL,
	span_id  TEXT    NOT NULL,
	ts       INTEGER NOT NULL,
	seq      INTEGER NOT NULL,
	kind     TEXT    NOT NULL,
	data     TEXT    NOT NULL,
	PRIMARY KEY (trace_id, span_id, seq)
);
CREATE INDEX IF NOT EXISTS events_idx ON events (trace_id, span_id, ts, seq);
CREATE TABLE IF NOT EXISTS memories (
	span_id TEXT PRIMARY KEY,
	data    TEXT NOT NULL
);
`

// openDefaultSQL opens (or reuses) the singleton SQLite database. Both the
// trace Store and the todo tools share this one handle so there is no lock
// contention between them.
func openDefaultSQL() (*sql.DB, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if defaultDB != nil {
		return defaultDB, nil
	}

	path, err := SqlitePath()
	if err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	if _, err := db.Exec(schemaDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	defaultDB = db
	return defaultDB, nil
}

// NewStore opens (or reuses) the singleton SQLite database (~/.toroid/sql.db).
func NewStore() (*Store, error) {
	db, err := openDefaultSQL()
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying shared *sql.DB (used to back the todo tools).
func (s *Store) DB() *sql.DB { return s.db }

// Close flushes WAL state. It intentionally does NOT close the shared singleton
// handle, since other live kernels may still be using it; the handle is released
// on process exit.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return nil
}

// UpdateTraceTitle patches only the title of an existing trace, preserving started_at.
func (s *Store) UpdateTraceTitle(traceID, title string) error {
	_, err := s.db.Exec(
		`INSERT INTO traces (trace_id, title) VALUES (?, ?)
		 ON CONFLICT(trace_id) DO UPDATE SET title = excluded.title`,
		traceID, title)
	return err
}

// SaveTraceMeta writes or updates trace metadata. started_at/ended_at are only
// overwritten when the incoming value is non-zero, so a later title-only save
// never clobbers the original start time.
func (s *Store) SaveTraceMeta(meta TraceMeta) error {
	_, err := s.db.Exec(
		`INSERT INTO traces (trace_id, title, started_at, ended_at, previous_trace_id) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(trace_id) DO UPDATE SET
		   title             = excluded.title,
		   started_at        = CASE WHEN excluded.started_at > 0 THEN excluded.started_at ELSE traces.started_at END,
		   ended_at          = CASE WHEN excluded.ended_at   > 0 THEN excluded.ended_at   ELSE traces.ended_at   END,
		   previous_trace_id = CASE WHEN excluded.previous_trace_id <> '' THEN excluded.previous_trace_id ELSE traces.previous_trace_id END`,
		meta.TraceID, meta.Title, meta.StartedAt, meta.EndedAt, meta.PreviousTraceID)
	return err
}

// LoadTraceMeta reads trace metadata by trace ID.
func (s *Store) LoadTraceMeta(traceID string) (TraceMeta, error) {
	var m TraceMeta
	row := s.db.QueryRow(`SELECT trace_id, title, started_at, ended_at, previous_trace_id FROM traces WHERE trace_id = ?`, traceID)
	err := row.Scan(&m.TraceID, &m.Title, &m.StartedAt, &m.EndedAt, &m.PreviousTraceID)
	if err == sql.ErrNoRows {
		return TraceMeta{}, nil
	}
	return m, err
}

// SaveSpanMeta writes or updates span metadata. Non-empty/non-zero fields update;
// blank fields preserve the existing value (so an end-of-run save that only sets
// ended_at keeps the model/title/started_at).
func (s *Store) SaveSpanMeta(meta SpanMeta) error {
	_, err := s.db.Exec(
		`INSERT INTO spans (trace_id, span_id, parent_span_id, model, title, started_at, ended_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(trace_id, span_id) DO UPDATE SET
		   parent_span_id = CASE WHEN excluded.parent_span_id <> '' THEN excluded.parent_span_id ELSE spans.parent_span_id END,
		   model          = CASE WHEN excluded.model          <> '' THEN excluded.model          ELSE spans.model END,
		   title          = CASE WHEN excluded.title          <> '' THEN excluded.title          ELSE spans.title END,
		   started_at     = CASE WHEN excluded.started_at      > 0  THEN excluded.started_at      ELSE spans.started_at END,
		   ended_at       = CASE WHEN excluded.ended_at        > 0  THEN excluded.ended_at        ELSE spans.ended_at END`,
		meta.TraceID, meta.SpanID, meta.ParentSpanID, meta.Model, meta.Title, meta.StartedAt, meta.EndedAt)
	return err
}

// LoadLastTotalUSD returns the total_usd from the most recent cost entry for a span, or 0.
func (s *Store) LoadLastTotalUSD(traceID, spanID string) float64 {
	var v float64
	row := s.db.QueryRow(
		`SELECT total_usd FROM costs WHERE trace_id = ? AND span_id = ? ORDER BY ts DESC LIMIT 1`,
		traceID, spanID)
	_ = row.Scan(&v)
	return v
}

// AppendCost records a turn cost under a span.
func (s *Store) AppendCost(traceID, spanID string, turnUSD, totalUSD float64) error {
	_, err := s.db.Exec(
		`INSERT INTO costs (trace_id, span_id, ts, turn_usd, total_usd) VALUES (?, ?, ?, ?, ?)`,
		traceID, spanID, time.Now().UnixNano(), turnUSD, totalUSD)
	return err
}

// AppendEvent records a session event under a span.
func (s *Store) AppendEvent(traceID, spanID string, event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO events (trace_id, span_id, ts, seq, kind, data) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(trace_id, span_id, seq) DO NOTHING`,
		traceID, spanID, event.EmitTS, event.Seq, string(event.Kind), string(data))
	return err
}

// LoadTraceTotal returns the sum of the last total_usd across all spans for a trace.
func (s *Store) LoadTraceTotal(traceID string) float64 {
	var total float64
	row := s.db.QueryRow(`
		SELECT COALESCE(SUM(t.total_usd), 0) FROM (
			SELECT c.total_usd
			FROM costs c
			JOIN (
				SELECT span_id, MAX(ts) AS mx FROM costs WHERE trace_id = ? GROUP BY span_id
			) m ON c.span_id = m.span_id AND c.ts = m.mx
			WHERE c.trace_id = ?
		) t`, traceID, traceID)
	_ = row.Scan(&total)
	return total
}

// SaveMemories writes the agent's persistent memory JSON blob for a span.
func (s *Store) SaveMemories(spanID string, mem map[string]any) error {
	b, err := json.MarshalIndent(mem, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO memories (span_id, data) VALUES (?, ?)
		 ON CONFLICT(span_id) DO UPDATE SET data = excluded.data`,
		spanID, string(b))
	return err
}

// LoadMemories reads the agent's persistent memory JSON blob for a span.
func (s *Store) LoadMemories(spanID string) (map[string]any, error) {
	var data string
	row := s.db.QueryRow(`SELECT data FROM memories WHERE span_id = ?`, spanID)
	err := row.Scan(&data)
	if err == sql.ErrNoRows {
		return map[string]any{}, nil
	}
	if err != nil {
		return map[string]any{}, err
	}
	mem := map[string]any{}
	_ = json.Unmarshal([]byte(data), &mem)
	return mem, nil
}

// CostEvent is a single turn cost record stored under a span.
type CostEvent struct {
	TS       int64   `json:"ts"` // UnixNano
	TurnUSD  float64 `json:"turn_usd"`
	TotalUSD float64 `json:"total_usd"`
}

// SpanData is a span with its cost events and session events, used for visualization.
type SpanData struct {
	SpanMeta
	Costs  []CostEvent `json:"costs"`
	Events []Event     `json:"events"`
}

// TraceData is the full trace for visualization.
type TraceData struct {
	Trace TraceMeta  `json:"trace"`
	Spans []SpanData `json:"spans"`
}

// LoadTraceData reads the full trace + all spans + costs + events for a trace ID.
func LoadTraceData(traceID string) (TraceData, error) {
	db, err := openDefaultSQL()
	if err != nil {
		return TraceData{}, err
	}
	var td TraceData

	row := db.QueryRow(`SELECT trace_id, title, started_at, ended_at, previous_trace_id FROM traces WHERE trace_id = ?`, traceID)
	if err := row.Scan(&td.Trace.TraceID, &td.Trace.Title, &td.Trace.StartedAt, &td.Trace.EndedAt, &td.Trace.PreviousTraceID); err != nil && err != sql.ErrNoRows {
		return td, err
	}

	spanRows, err := db.Query(
		`SELECT span_id, parent_span_id, model, title, started_at, ended_at
		 FROM spans WHERE trace_id = ? ORDER BY started_at, span_id`, traceID)
	if err != nil {
		return td, err
	}
	defer spanRows.Close()

	for spanRows.Next() {
		var sd SpanData
		sd.TraceID = traceID
		if err := spanRows.Scan(&sd.SpanID, &sd.ParentSpanID, &sd.Model, &sd.Title, &sd.StartedAt, &sd.EndedAt); err != nil {
			return td, err
		}
		td.Spans = append(td.Spans, sd)
	}
	if err := spanRows.Err(); err != nil {
		return td, err
	}

	for i := range td.Spans {
		sp := &td.Spans[i]
		if costs, err := loadSpanCosts(db, traceID, sp.SpanID); err == nil {
			sp.Costs = costs
		}
		if events, err := loadSpanEvents(db, traceID, sp.SpanID); err == nil {
			sp.Events = events
		}
	}
	return td, nil
}

func loadSpanCosts(db *sql.DB, traceID, spanID string) ([]CostEvent, error) {
	rows, err := db.Query(`SELECT ts, turn_usd, total_usd FROM costs WHERE trace_id = ? AND span_id = ? ORDER BY ts`, traceID, spanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostEvent
	for rows.Next() {
		var c CostEvent
		if err := rows.Scan(&c.TS, &c.TurnUSD, &c.TotalUSD); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadSpanEvents(db *sql.DB, traceID, spanID string) ([]Event, error) {
	rows, err := db.Query(`SELECT data FROM events WHERE trace_id = ? AND span_id = ? ORDER BY seq`, traceID, spanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var ev Event
		if err := json.Unmarshal([]byte(data), &ev); err == nil {
			out = append(out, ev)
		}
	}
	return out, rows.Err()
}

// SessionInfo holds metadata for listing traces/sessions.
type SessionInfo struct {
	ID          string // trace ID (root span ID)
	Title       string
	StartedAt   int64   // UnixNano
	DurationNs  int64   // last cost event ts - started_at (wall time)
	AgentTimeNs int64   // sum of per-step durations
	TotalUSD    float64 // sum of all turn_usd across all spans
}

func (s SessionInfo) StartedAtFmt() string {
	return time.Unix(0, s.StartedAt).Format("Jan 02, 15:04:05")
}

func fmtDuration(ns int64) string {
	if ns <= 0 {
		return "—"
	}
	d := time.Duration(ns)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (s SessionInfo) DurationFmt() string  { return fmtDuration(s.DurationNs) }
func (s SessionInfo) AgentTimeFmt() string { return fmtDuration(s.AgentTimeNs) }

// ListSessions returns all traces sorted newest first.
func ListSessions() ([]SessionInfo, error) {
	db, err := openDefaultSQL()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT trace_id, title, started_at FROM traces ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var infos []SessionInfo
	for rows.Next() {
		var info SessionInfo
		if err := rows.Scan(&info.ID, &info.Title, &info.StartedAt); err != nil {
			return nil, err
		}
		if info.Title == "" {
			info.Title = "(no title)"
		}
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Enrich each session: total cost, wall duration, and agent (per-step) time.
	for i := range infos {
		info := &infos[i]

		costRow := db.QueryRow(
			`SELECT COALESCE(SUM(turn_usd), 0), COALESCE(MAX(ts), 0) FROM costs WHERE trace_id = ?`, info.ID)
		var maxTS int64
		_ = costRow.Scan(&info.TotalUSD, &maxTS)
		if maxTS > info.StartedAt && info.StartedAt > 0 {
			info.DurationNs = maxTS - info.StartedAt
		}

		// Agent time: sum of step durations (UserPromptSubmit → TurnCost) per span.
		spanRows, err := db.Query(`SELECT span_id FROM spans WHERE trace_id = ?`, info.ID)
		if err != nil {
			continue
		}
		var spanIDs []string
		for spanRows.Next() {
			var sid string
			if err := spanRows.Scan(&sid); err == nil {
				spanIDs = append(spanIDs, sid)
			}
		}
		spanRows.Close()
		for _, sid := range spanIDs {
			events, err := loadSpanEvents(db, info.ID, sid)
			if err != nil {
				continue
			}
			var stepStart int64
			for _, e := range events {
				switch e.Kind {
				case EventUserPromptSubmit:
					stepStart = e.EmitTS
				case EventTurnCost:
					if stepStart > 0 {
						info.AgentTimeNs += e.EmitTS - stepStart
					}
					stepStart = e.EmitTS
				}
			}
		}
	}
	return infos, nil
}

// DeleteSession removes all data associated with a trace ID.
func DeleteSession(id string) error {
	db, err := openDefaultSQL()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM events WHERE trace_id = ?`,
		`DELETE FROM costs  WHERE trace_id = ?`,
		`DELETE FROM spans  WHERE trace_id = ?`,
		`DELETE FROM traces WHERE trace_id = ?`,
		`DELETE FROM memories WHERE span_id = ?`,
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// OpenTelemetry-native telemetry (§12.2).
//
// The kernel's persisted trace/span graph maps directly onto OpenTelemetry: a
// span is a kernel run, child spans are subagents, and kernel Events become span
// events. There is ONE canonical OTEL shape, and it is derived on read from the
// raw Events stored under Save:true — the DB keeps full-fidelity Events and
// OTELSpans projects them losslessly (payloads land in span-event attributes,
// token usage and cost in GenAI semantic-convention span attributes). Because the
// projection is the single source of truth, the persisted and exported views can
// never drift. A host feeds these snapshots to any OTLP exporter without the
// kernel taking a hard dependency on the OpenTelemetry SDK.

// OTELEvent is a single event recorded against a span (an OTEL span event).
type OTELEvent struct {
	Name      string `json:"name"`
	TimeUnix  int64  `json:"time_unix_nano"`
	Attribute string `json:"attribute,omitempty"` // JSON payload, if any
}

// OTELKeyValue is a single OpenTelemetry span attribute. Value is a scalar
// (string, int64, float64, or bool) per the OTEL attribute model.
type OTELKeyValue struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// OTELSpan is an OpenTelemetry-shaped view of a kernel span.
type OTELSpan struct {
	TraceID      oteltrace.TraceID `json:"trace_id"`
	SpanID       oteltrace.SpanID  `json:"span_id"`
	ParentSpanID oteltrace.SpanID  `json:"parent_span_id"`
	Name         string            `json:"name"`
	StartUnix    int64             `json:"start_unix_nano"`
	EndUnix      int64             `json:"end_unix_nano"`
	Model        string            `json:"model,omitempty"`      // convenience mirror of gen_ai.request.model
	CostUSD      float64           `json:"cost_usd"`             // convenience mirror of gen_ai.usage.cost_usd
	Attributes   []OTELKeyValue    `json:"attributes,omitempty"` // GenAI semantic-convention attributes
	Events       []OTELEvent       `json:"events,omitempty"`
}

// OTELSpans loads a trace and returns its spans as OpenTelemetry-shaped snapshots
// with spec-valid trace/span IDs and start/end timestamps.
func OTELSpans(traceID string) ([]OTELSpan, error) {
	td, err := LoadTraceData(traceID)
	if err != nil {
		return nil, err
	}
	out := make([]OTELSpan, 0, len(td.Spans))
	for _, sp := range td.Spans {
		tid, sid := OTELIDs(sp.TraceID, sp.SpanID)
		var pid oteltrace.SpanID
		if sp.ParentSpanID != "" {
			_, pid = OTELIDs(sp.TraceID, sp.ParentSpanID)
		}

		name := sp.Title
		if name == "" {
			name = "kernel.run"
		}

		var cost float64
		if n := len(sp.Costs); n > 0 {
			cost = sp.Costs[n-1].TotalUSD // total_usd is cumulative; last row is the span total
		}

		// Span events: the single canonical Event->OTEL mapping, filtered to
		// observability-relevant kinds (drops display/control-plane chatter).
		events := make([]OTELEvent, 0, len(sp.Events))
		for _, ev := range sp.Events {
			if !ev.Kind.Observable() {
				continue
			}
			events = append(events, ev.OTEL())
		}

		out = append(out, OTELSpan{
			TraceID:      tid,
			SpanID:       sid,
			ParentSpanID: pid,
			Name:         name,
			StartUnix:    sp.StartedAt,
			EndUnix:      sp.EndedAt,
			Model:        sp.Model,
			CostUSD:      cost,
			Attributes:   spanAttributes(sp, cost),
			Events:       events,
		})
	}
	return out, nil
}

// spanAttributes builds the OTEL GenAI semantic-convention attributes for a span:
// the request model plus aggregated token usage and cost. These are what let
// backends like Langfuse or SigNoz render model/token/cost dashboards natively.
func spanAttributes(sp SpanData, cost float64) []OTELKeyValue {
	attrs := make([]OTELKeyValue, 0, 8)
	if sp.Model != "" {
		attrs = append(attrs, OTELKeyValue{Key: "gen_ai.request.model", Value: sp.Model})
	}

	u := aggregateUsage(sp)
	addTokens := func(key string, n int64) {
		if n > 0 {
			attrs = append(attrs, OTELKeyValue{Key: key, Value: n})
		}
	}
	addTokens("gen_ai.usage.input_tokens", u.Input)
	addTokens("gen_ai.usage.output_tokens", u.Output)
	addTokens("gen_ai.usage.reasoning_tokens", u.Reasoning)
	addTokens("gen_ai.usage.cache_read_tokens", u.CacheRead)
	addTokens("gen_ai.usage.cache_write_tokens", u.CacheWrite)
	addTokens("gen_ai.usage.total_tokens", u.Input+u.Output)

	attrs = append(attrs, OTELKeyValue{Key: "gen_ai.usage.cost_usd", Value: cost})
	return attrs
}

// aggregateUsage sums per-turn token usage recorded on a span's TurnCost events.
// Loaded events carry Payload as a decoded map, so it is re-marshaled into the
// typed payload rather than type-asserted.
func aggregateUsage(sp SpanData) Usage {
	var total Usage
	for _, ev := range sp.Events {
		if ev.Kind != EventTurnCost || ev.Payload == nil {
			continue
		}
		var p TurnCostPayload
		b, err := json.Marshal(ev.Payload)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(b, &p); err != nil {
			continue
		}
		total.Input += p.TurnUsage.Input
		total.Output += p.TurnUsage.Output
		total.Reasoning += p.TurnUsage.Reasoning
		total.CacheRead += p.TurnUsage.CacheRead
		total.CacheWrite += p.TurnUsage.CacheWrite
	}
	return total
}
