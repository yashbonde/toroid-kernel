package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	_ "modernc.org/sqlite"
)

// runWatch implements `trk watch`. It serves a read-only HTTP view of the
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
	handler.HandleFunc("/api/db", watchDB)
	handler.HandleFunc("/api/sessions", watchSessions)
	handler.HandleFunc("/api/traces", watchJSON(func() (any, error) { return listTraces() }))
	handler.HandleFunc("/api/traces/", watchTrace)

	// A fixed port could already be in use (another `trk watch`, or any app);
	// asking the kernel for port 0 hands back a random free loopback port, which
	// is what "random fixed port" needs: stable for this process, never colliding.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()

	fmt.Fprintf(out, "trk watch serving http://%s\n", ln.Addr().String())
	fmt.Fprintf(out, "database: %s\n", dbPath)
	fmt.Fprintln(out, "press Ctrl-C to stop")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	_ = server.Close()
	return nil
}

// watchIndex is a tiny human-facing landing page linking to the JSON views.
func watchIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html>
<html>
<head><meta charset="utf-8"><title>trk watch</title></head>
<body>
<h1>trk watch</h1>
<ul>
	<li><a href="/api/db">/api/db</a> — raw database value dump</li>
	<li><a href="/api/sessions">/api/sessions</a> — session summary</li>
	<li><a href="/api/traces">/api/traces</a> — all traces</li>
	<li><a href="/api/traces/&lt;id&gt;">/api/traces/&lt;id&gt;</a> — one trace</li>
</ul>
</body>
</html>
`)
}

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

// writeWatchJSON pretty-prints v to the response so a browser read is legible
// without any client-side JS.
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

// watchDB dumps every table in the store as a raw JSON document: one key per
// table, each an array of rows (columns preserved verbatim). This is the
// "show all the values" view.
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
