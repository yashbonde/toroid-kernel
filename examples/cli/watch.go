package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	_ "modernc.org/sqlite"
)

// runWatch implements `trk watch`. It serves a read-only HTTP dashboard of the
// Toroid SQLite store (sessions, traces, spans, costs, events, memories) on a
// loopback port chosen by the OS so it can never collide with another process.
// The database path is resolved through toroid.SqlitePath(), so TOROID_HOME is
// honored exactly like every other subcommand.
func runWatch(out io.Writer, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("takes no arguments")
	}

	dbPath, err := toroid.SqlitePath()
	if err != nil {
		return err
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/", watchIndex)
	handler.HandleFunc("/trace/", watchTracePage)
	handler.HandleFunc("/api/db", watchDB)
	handler.HandleFunc("/api/sessions", watchSessions)
	handler.HandleFunc("/api/traces", watchJSON(func() (any, error) { return listTraces() }))
	handler.HandleFunc("/api/traces/", watchTrace)

	// Fixed loopback port so the dashboard URL is stable across restarts.
	ln, err := net.Listen("tcp", "127.0.0.1:51465")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()

	fmt.Fprintf(out, "trk watch dashboard: http://%s\n", ln.Addr().String())
	fmt.Fprintf(out, "database: %s\n", dbPath)
	fmt.Fprintln(out, "press Ctrl-C to stop")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	_ = server.Close()
	return nil
}

// ---------------------------------------------------------------------------
// HTML dashboard
// ---------------------------------------------------------------------------

// watchCSS is the shared dark, minimal, system-font stylesheet.
const watchCSS = `
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body { margin: 0; font: 14px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0e1116; color: #d7dde5; }
.wrap { max-width: 980px; margin: 0 auto; padding: 32px 24px 80px; }
header { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 24px; gap: 12px; flex-wrap: wrap; }
h1 { font-size: 21px; margin: 0; font-weight: 650; letter-spacing: -.01em; }
h1 .dot { color: #4ade80; }
.breadcrumb { color: #687382; font-size: 12px; }
.breadcrumb a { color: #7d8590; text-decoration: none; }
.breadcrumb a:hover { color: #d7dde5; }
h2 { font-size: 15px; margin: 0 0 10px; font-weight: 600; color: #9aa6b2; }
.cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; margin-bottom: 30px; }
.card { background: #161b22; border: 1px solid #232a33; border-radius: 10px; padding: 15px 16px; }
.card .label { font-size: 11px; color: #7d8590; text-transform: uppercase; letter-spacing: .06em; margin-bottom: 6px; }
.card .value { font-size: 22px; font-weight: 650; }
.card .value.small { font-size: 16px; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid #1c232c; }
th { font-size: 11px; color: #7d8590; text-transform: uppercase; letter-spacing: .06em; font-weight: 500; }
tr.row:hover td { background: #151b23; }
td.num, td.cost { font-variant-numeric: tabular-nums; }
td.cost { text-align: right; color: #4ade80; }
span.muted { color: #687382; }
.chip { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 11px; background: #161b22; color: #9aa6b2; border: 1px solid #232a33; }
section { margin-bottom: 34px; }
.span-head { display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap; margin-bottom: 6px; }
.span-title { font-weight: 600; }
.tool { display: flex; gap: 10px; align-items: baseline; padding: 7px 0; border-bottom: 1px solid #1c232c; }
.tool .n { color: #687382; width: 24px; text-align: right; flex: none; font-variant-numeric: tabular-nums; font-size: 12px; }
.tool .name { color: #7ee2ff; font-family: ui-monospace, Menlo, monospace; }
.tool .args { color: #687382; font-family: ui-monospace, Menlo, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
details.tl { border-bottom: 1px solid #1c232c; }
details.tl > summary { list-style: none; cursor: pointer; display: flex; gap: 10px; align-items: baseline; padding: 7px 0; }
details.tl > summary::-webkit-details-marker { display: none; }
details.tl > summary:hover { background: #151b23; }
details.tl[open] > summary { background: #151b23; }
.tl .badge { flex: none; width: 74px; font-size: 10px; text-transform: uppercase; letter-spacing: .06em; font-weight: 600; color: #7d8590; }
.badge.user { color: #7ee2ff; }
.badge.assistant { color: #4ade80; }
.badge.thinking { color: #f0b16e; }
.badge.tool { color: #c9a2ff; }
.badge.tool-result { color: #7d8590; }
.tl .summary { color: #9fb0c3; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tl .body { color: #9fb0c3; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12.5px; white-space: pre-wrap; word-break: break-word; padding: 0 0 12px 84px; }
.tl .body.empty { color: #4d5560; }
.empty { color: #4d5560; font-style: italic; padding: 18px 0; }
a { color: #58a6ff; text-decoration: none; }
a:hover { text-decoration: underline; }
`

// tplFuncs are helper functions available to every dashboard template.
var tplFuncs = template.FuncMap{
	"timeFmt": func(ns int64) string {
		if ns <= 0 {
			return "—"
		}
		return time.Unix(0, ns).Format("02 Jan 15:04:05")
	},
	"durFmt": func(ns int64) string {
		if ns <= 0 {
			return "—"
		}
		d := time.Duration(ns)
		if d < time.Second {
			return fmt.Sprintf("%dms", d.Milliseconds())
		}
		return fmt.Sprintf("%.1fs", d.Seconds())
	},
	"moneyFmt": func(v float64) string {
		return fmt.Sprintf("$%.6f", v)
	},
	"shortID": func(id string) string {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	},
	"addInt": func(a, b int) int { return a + b },
	"toolCount": func(events []toroid.Event) int {
		n := 0
		for _, e := range events {
			if e.Kind == toroid.EventPreToolUse {
				n++
			}
		}
		return n
	},
	"toolName": func(e toroid.Event) string {
		return fieldString(e.Payload, "name")
	},
	"toolArgs": func(e toroid.Event) string {
		return fieldString(e.Payload, "args")
	},
	"timelineOf": timelineOf,
	"labelClass": func(label string) string {
		switch label {
		case "user":
			return "user"
		case "assistant":
			return "assistant"
		case "thinking":
			return "thinking"
		case "tool":
			return "tool"
		case "tool-result":
			return "tool-result"
		default:
			return ""
		}
	},
}

// fieldString extracts a string field from an event payload that may arrive
// either as a typed *ToolUsePayload (live hooks) or as a generic map[string]any
// (decoded from the persisted JSON blob).
func fieldString(payload any, key string) string {
	switch p := payload.(type) {
	case *toroid.ToolUsePayload:
		if key == "name" {
			return p.Name
		}
		return p.Args
	case map[string]any:
		if v, ok := p[key].(string); ok {
			return v
		}
	case *map[string]any:
		if v, ok := (*p)[key].(string); ok {
			return v
		}
	}
	return ""
}

// spanTimelineEntry is one rendered row of a span's conversation timeline:
// user prompt, assistant text, thinking/reasoning, tool call, or tool result.
// Title is the collapsed summary; Body is the full content shown on expand.
type spanTimelineEntry struct {
	Label string // user | assistant | thinking | tool | tool-result
	Title string
	Body  string
}

// timelineOf flattens a span's events into a chronological conversation
// timeline. User prompts come from UserPromptSubmit events; assistant text,
// thinking, tool calls and tool results come from the structured content the
// turn persists (TurnCompleted's content / the legacy AssistantTurn messages).
// The decoded payload is a generic map[string]any (not the typed struct), so
// every field is read defensively.
func timelineOf(sp toroid.SpanData) []spanTimelineEntry {
	var out []spanTimelineEntry
	for _, e := range sp.Events {
		switch e.Kind {
		case toroid.EventUserPromptSubmit:
			if p := payloadString(e.Payload, "prompt"); p != "" {
				out = append(out, spanTimelineEntry{Label: "user", Title: summarize(p, 120), Body: p})
			}
		case toroid.EventTurnCompleted, toroid.EventKind("AssistantTurn"):
			out = append(out, timelineFromMessages(messagesOf(e.Payload))...)
		}
	}
	return out
}

// messagesOf extracts the ordered []Message from a TurnPayload/AssistantTurn
// payload, accepting both the "content" (TurnCompleted) and "messages"
// (legacy AssistantTurn) keys.
func messagesOf(payload any) []map[string]any {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"content", "messages"} {
		raw, ok := m[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(raw))
		for _, c := range raw {
			if cm, ok := c.(map[string]any); ok {
				out = append(out, cm)
			}
		}
		return out
	}
	return nil
}

// timelineFromMessages turns a persisted message list into timeline rows.
func timelineFromMessages(msgs []map[string]any) []spanTimelineEntry {
	var out []spanTimelineEntry
	for _, msg := range msgs {
		role, _ := msg["role"].(string)
		parts, _ := msg["parts"].([]any)
		for _, p := range partMaps(parts) {
			kind := jsonStr(p["kind"])
			switch role {
			case "user":
				if kind == "text" {
					t := jsonStr(p["text"])
					out = append(out, spanTimelineEntry{Label: "user", Title: summarize(t, 120), Body: t})
				}
			case "assistant":
				switch kind {
				case "text":
					t := jsonStr(p["text"])
					out = append(out, spanTimelineEntry{Label: "assistant", Title: summarize(t, 120), Body: t})
				case "reasoning":
					t := jsonStr(p["text"])
					out = append(out, spanTimelineEntry{Label: "thinking", Title: summarize(t, 120), Body: t})
				case "tool_call":
					name, args := jsonStr(p["name"]), jsonStr(p["arguments"])
					title := name
					if s := summarize(args, 80); s != "" {
						title = name + "  " + s
					}
					out = append(out, spanTimelineEntry{Label: "tool", Title: title, Body: args})
				}
			case "tool":
				if kind == "tool_result" {
					c := jsonStr(p["content"])
					out = append(out, spanTimelineEntry{Label: "tool-result", Title: summarize(c, 100), Body: c})
				}
			}
		}
	}
	return out
}

func partMaps(parts []any) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			out = append(out, pm)
		}
	}
	return out
}

func jsonStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func payloadString(payload any, key string) string {
	if m, ok := payload.(map[string]any); ok {
		return jsonStr(m[key])
	}
	return ""
}

// summarize collapses a string to a single-line, width-bounded summary.
func summarize(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n-1]) + "…"
	}
	return s
}

func newPageTemplate(body string) *template.Template {
	return template.Must(template.New("page").Funcs(tplFuncs).Parse(
		`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>trk watch</title><style>` +
			watchCSS + `</style></head><body><div class="wrap">` + body + `</div></body></html>`))
}

var indexTpl = newPageTemplate(`
<header><h1><span class="dot">●</span> trk watch</h1><span class="breadcrumb">db: {{ .DBPath }}</span></header>
<div class="cards">
  <div class="card"><div class="label">Sessions</div><div class="value">{{ .Count }}</div></div>
  <div class="card"><div class="label">Total spend</div><div class="value">{{ .Total | moneyFmt }}</div></div>
  <div class="card"><div class="label">Agent time</div><div class="value small">{{ .AgentNS | durFmt }}</div></div>
</div>
{{ if .Sessions }}
<table>
  <thead><tr><th>Session</th><th>Title</th><th>Started</th><th>Duration</th><th style="text-align:right">Cost</th></tr></thead>
  <tbody>
  {{ range .Sessions }}
    <tr class="row"><td class="num"><a href="/trace/{{ .ID }}">{{ .ID | shortID }}</a></td><td>{{ .Title }}</td><td class="num">{{ .StartedAt | timeFmt }}</td><td class="num">{{ .DurationNs | durFmt }}</td><td class="cost">{{ .TotalUSD | moneyFmt }}</td></tr>
  {{ end }}
  </tbody>
</table>
{{ else }}
<p class="empty">No sessions yet — run the agent with --save to populate the dashboard.</p>
{{ end }}
`)

type indexData struct {
	DBPath   string
	Sessions []toroid.SessionInfo
	Count    int
	Total    float64
	AgentNS  int64
}

// watchIndex is the human-facing dashboard landing page: stat cards plus a
// session table, each row linking to its trace detail page. Raw JSON stays
// available under /api/*.
func watchIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	sessions, err := toroid.ListSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := indexData{Sessions: sessions, Count: len(sessions)}
	for _, s := range sessions {
		d.Total += s.TotalUSD
		d.AgentNS += s.AgentTimeNs
	}
	if p, err := toroid.SqlitePath(); err == nil {
		d.DBPath = p
	}
	renderPage(w, indexTpl, d)
}

var traceTpl = newPageTemplate(`
<header><h1><span class="dot">●</span> trk watch</h1><span class="breadcrumb"><a href="/">← sessions</a></span></header>
<div class="span-head"><h2 style="margin:0">{{ if .Trace.Title }}{{ .Trace.Title }}{{ else }}<span class="muted">(untitled)</span>{{ end }}</h2><span class="chip">{{ .Trace.TraceID | shortID }}</span></div>
<div class="cards">
  <div class="card"><div class="label">Started</div><div class="value small">{{ .Trace.StartedAt | timeFmt }}</div></div>
  <div class="card"><div class="label">Ended</div><div class="value small">{{ .Trace.EndedAt | timeFmt }}</div></div>
  <div class="card"><div class="label">Cost</div><div class="value">{{ .Cost | moneyFmt }}</div></div>
  <div class="card"><div class="label">Spans</div><div class="value">{{ len .Spans }}</div></div>
</div>
{{ if .Spans }}
{{ range .Spans }}
{{ $tl := . | timelineOf }}
<section>
  <div class="span-head"><span class="span-title">{{ if .Title }}{{ .Title }}{{ else }}<span class="muted">span</span>{{ end }}</span><span class="chip">{{ .SpanID | shortID }}</span>{{ if .Model }}<span class="muted">{{ .Model }}</span>{{ end }}</div>
  <div class="breadcrumb" style="margin-bottom:6px">{{ .Events | toolCount }} tool calls · {{ len .Costs }} cost records</div>
  {{ if $tl }}
    {{ range $tl }}
    <details class="tl">
      <summary><span class="badge {{ .Label | labelClass }}">{{ .Label }}</span><span class="summary">{{ .Title }}</span></summary>
      <div class="body">{{ if .Body }}{{ .Body }}{{ else }}<span class="empty">(empty)</span>{{ end }}</div>
    </details>
    {{ end }}
  {{ else }}
  <p class="empty">no conversation recorded</p>
  {{ end }}
</section>
{{ end }}
{{ else }}
<p class="empty">no spans recorded for this trace</p>
{{ end }}
`)

type tracePageData struct {
	Trace toroid.TraceMeta
	Spans []toroid.SpanData
	Cost  float64
}

// watchTracePage renders the HTML detail page for one trace.
func watchTracePage(w http.ResponseWriter, r *http.Request) {
	id := trimPrefixPath(r.URL.Path, "/trace/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	td, err := toroid.LoadTraceData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if td.Trace.TraceID == "" {
		http.NotFound(w, r)
		return
	}
	var cost float64
	for _, sp := range td.Spans {
		for _, c := range sp.Costs {
			if c.TotalUSD > cost {
				cost = c.TotalUSD
			}
		}
	}
	renderPage(w, traceTpl, tracePageData{Trace: td.Trace, Spans: td.Spans, Cost: cost})
}

// renderPage executes tpl against data, setting the HTML content type.
func renderPage(w http.ResponseWriter, tpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.Execute(w, data); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// JSON API (unchanged for scripted consumers)
// ---------------------------------------------------------------------------

// watchJSON wraps a handler whose result is JSON-encoded with pretty-printing.
func watchJSON(fn func() (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeWatchJSON(w, v)
	}
}

// writeWatchJSON pretty-prints v to the response.
func writeWatchJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// watchSessions serves the same session list `trk sessions` prints.
func watchSessions(w http.ResponseWriter, _ *http.Request) {
	sessions, err := toroid.ListSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []toroid.SessionInfo{}
	}
	writeWatchJSON(w, sessions)
}

// watchTrace serves one trace (or an error for an unknown id).
func watchTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := trimPrefixPath(r.URL.Path, "/api/traces/")
	if id == "" {
		http.Error(w, "missing trace id", http.StatusBadRequest)
		return
	}
	td, err := toroid.LoadTraceData(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if td.Trace.TraceID == "" {
		http.Error(w, "no such trace", http.StatusNotFound)
		return
	}
	writeWatchJSON(w, td)
}

func trimPrefixPath(path, prefix string) string {
	if len(path) > len(prefix) {
		return path[len(prefix):]
	}
	return ""
}

// listTraces returns every persisted trace (id + title), oldest last.
func listTraces() (any, error) {
	db, err := openWatchDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT trace_id, title, started_at FROM traces ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		ID        string `json:"trace_id"`
		Title     string `json:"title"`
		StartedAt int64  `json:"started_at"`
	}
	out := []row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Title, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// openWatchDB opens a second, read-only handle to the Toroid SQLite database so
// the server can read the current on-disk state even while a kernel (or another
// `trk watch`) holds the primary handle open.
func openWatchDB() (*sql.DB, error) {
	path, err := toroid.SqlitePath()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	return db, db.Ping()
}

// watchDB dumps every table in the store as a raw JSON document for machines.
func watchDB(w http.ResponseWriter, r *http.Request) {
	db, err := openWatchDB()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	tables := []string{"traces", "spans", "costs", "events", "memories"}
	doc := make(map[string]any, len(tables))
	for _, table := range tables {
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			doc[table] = map[string]string{"error": err.Error()}
			continue
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			doc[table] = map[string]string{"error": err.Error()}
			continue
		}
		var data []map[string]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				doc[table] = map[string]string{"error": err.Error()}
				continue
			}
			rec := make(map[string]any, len(cols))
			for i, col := range cols {
				rec[col] = normWatchValue(vals[i])
			}
			data = append(data, rec)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			doc[table] = map[string]string{"error": err.Error()}
			continue
		}
		if data == nil {
			data = []map[string]any{}
		}
		doc[table] = data
	}
	writeWatchJSON(w, doc)
}

// normWatchValue converts driver values into JSON-friendly scalars.
func normWatchValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}
