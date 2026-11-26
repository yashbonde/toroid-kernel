// Command webui serves Toroid's local, single-user chat workspace.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

//go:embed static/*
var assets embed.FS

type app struct {
	db              *sql.DB
	base            agent
	workdir         string
	maxIter         int
	host, launchKey string
	active          sync.Map // one in-flight request per transcript
	newKernel       func(context.Context, toroid.Config) (*toroid.Kernel, error)
}

func main() {
	port := flag.Int("port", 8787, "local HTTP port (0 for an available port)")
	flag.Parse()
	store, err := toroid.NewStore()
	if err != nil {
		log.Fatal(err)
	}
	if _, err = store.DB().Exec(webSchema); err != nil {
		log.Fatal(err)
	}
	model := os.Getenv("TOROID_MODEL")
	if model == "" {
		model = toroid.DefaultModel
	}
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	maxIter, _ := strconv.Atoi(os.Getenv("TOROID_MAX_ITER"))
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		log.Fatal(err)
	}
	a := &app{
		db:      store.DB(),
		workdir: wd,
		maxIter: maxIter,
		host:    l.Addr().String(),
		base: agent{ID: "base", Name: "base", Settings: settings{
			Model: model, Thinking: toroid.ThinkingLow, Computer: true, Skills: true, Delegate: true,
		}},
		launchKey: rand.Text(),

		newKernel: toroid.NewKernel,
	}
	token := os.Getenv("TOROID_LLM_TOKEN")
	if !strings.HasPrefix(model, "openai/") || token != "" || os.Getenv(toroid.OpenAIKeyEnv) != "" {
		toroid.WarmModelMetadata(toroid.Config{Model: model, APIKey: token, IncludeSubagentTools: a.base.Settings.Delegate})
	}
	fmt.Printf("Open Toroid  http://%s/#%s\n", a.host, a.launchKey)
	srv := http.Server{
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Fatal(srv.Serve(l))
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/unlock", func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Key string }
		if !decode(w, r, &input) {
			return
		}
		if !equal(input.Key, a.launchKey) {
			http.Error(w, "Open the link printed by the server.", 403)
			return
		}
		token := rand.Text()
		digest := sha256.Sum256([]byte(token))
		expires := time.Now().Add(30 * 24 * time.Hour)
		if _, err := a.db.ExecContext(r.Context(), `INSERT INTO webui_browser_sessions(digest,host,expires) VALUES(?,?,?)`, digest[:], a.host, expires.Unix()); err != nil {
			dbError(w, err)
			return
		}
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM webui_browser_sessions WHERE expires<=?`, time.Now().Unix())
		http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: token, Path: "/api/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: expires})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/workspace", a.workspace)
	mux.HandleFunc("GET /api/models", a.models)
	mux.HandleFunc("GET /api/folders", a.folders)
	mux.HandleFunc("POST /api/attachments", a.attachments)
	mux.HandleFunc("GET /api/attachments/preview", a.preview)
	mux.HandleFunc("DELETE /api/agents/{id}", a.deleteAgent)
	mux.HandleFunc("POST /api/agents", a.saveAgent)
	mux.HandleFunc("GET /api/chats/{id}", a.detail)
	mux.HandleFunc("PUT /api/chats/{id}/pin", a.pin)
	mux.HandleFunc("POST /api/turns", a.turn)
	mux.HandleFunc("GET /api/chats/{id}/stream/{run}", a.follow)
	mux.HandleFunc("DELETE /api/chats/{id}/stream/{run}", a.stop)
	mux.HandleFunc("PUT /api/chats/{id}/seen", a.seen)
	mux.HandleFunc("PUT /api/chats/{id}/title", a.rename)
	mux.HandleFunc("POST /api/chats/{id}/fork", a.fork)
	files, _ := fs.Sub(assets, "static")
	mux.Handle("GET /", http.FileServerFS(files))
	slots := make(chan struct{}, 12)
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; frame-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
		if r.Host != a.host || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+a.host) {
			http.Error(w, "Local requests only.", 403)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.URL.Path != "/api/unlock" {
				c, err := r.Cookie(a.cookieName())
				if err != nil || !a.paired(r, c.Value) {
					http.Error(w, "Open the link printed by the server.", 401)
					return
				}
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				http.Error(w, "Server busy. Try again shortly.", 429)
				return
			}
		}
		protected.ServeHTTP(w, r)
	})
}

func (a *app) cookieName() string {
	_, port, _ := net.SplitHostPort(a.host)
	return "toroid_webui_" + port
}

func (a *app) paired(r *http.Request, token string) bool {
	digest := sha256.Sum256([]byte(token))
	var one int
	return a.db.QueryRowContext(r.Context(), `SELECT 1 FROM webui_browser_sessions WHERE digest=? AND host=? AND expires>?`, digest[:], a.host, time.Now().Unix()).Scan(&one) == nil
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		http.Error(w, "Expected JSON.", 415)
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "Invalid or oversized request.", 400)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func dbError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Chat or agent not found.", 404)
		return
	}
	http.Error(w, "Could not read or write the Toroid database.", 500)
}

func (a *app) workspace(w http.ResponseWriter, r *http.Request) {
	chats, err := a.chats(r.Context())
	if err != nil {
		dbError(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,name,config FROM webui_agents ORDER BY name COLLATE NOCASE`)
	if err != nil {
		dbError(w, err)
		return
	}
	defer rows.Close()
	agents := []agent{a.base}
	for rows.Next() {
		var item agent
		var raw []byte
		if err = rows.Scan(&item.ID, &item.Name, &raw); err != nil {
			dbError(w, err)
			return
		}
		if err = json.Unmarshal(raw, &item.Settings); err != nil {
			dbError(w, err)
			return
		}
		agents = append(agents, item)
	}
	if err = rows.Err(); err != nil {
		dbError(w, err)
		return
	}
	reply(w, map[string]any{"chats": chats, "agents": agents, "cwd": a.workdir})
}

func (a *app) detail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name, draft, err := a.chatProfile(r.Context(), id)
	if err != nil {
		dbError(w, err)
		return
	}
	var ready int64
	if err := a.db.QueryRowContext(r.Context(), `SELECT COALESCE((SELECT ready FROM webui_replies WHERE trace_id=?),0)`, id).Scan(&ready); err != nil {
		dbError(w, err)
		return
	}
	through := int64(math.MaxInt64)
	stream := ""
	if run := a.live(id); run != nil && r.URL.Query().Has("live") {
		through, stream = run.before, run.id
	}
	m, err := readMessages(r.Context(), a.db, id, through)
	if err != nil {
		dbError(w, err)
		return
	}
	o, err := a.options(r, id)
	if err != nil {
		dbError(w, err)
		return
	}
	usage, err := a.savedUsage(r.Context(), id)
	if err != nil {
		dbError(w, err)
		return
	}
	reply(w, map[string]any{"ready": ready, "stream": stream, "messages": m, "agent": name, "draft": draft, "options": o, "usage": usage})
}

func (a *app) pin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Pinned *bool }
	if !decode(w, r, &req) {
		return
	}
	if req.Pinned == nil {
		http.Error(w, "Choose a pin state.", 400)
		return
	}
	id := r.PathValue("id")
	if _, _, err := a.chatProfile(r.Context(), id); err != nil {
		dbError(w, err)
		return
	}
	_, err := a.db.ExecContext(r.Context(), `INSERT INTO webui_chats(trace_id,pinned) VALUES(?,?)
		ON CONFLICT(trace_id) DO UPDATE SET pinned=excluded.pinned`, id, *req.Pinned)
	if err != nil {
		dbError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *app) saveAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string
		Settings settings
		Preview  string
	}
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 60 || strings.EqualFold(req.Name, "base") || !req.Settings.valid() {
		http.Error(w, "Choose a unique name and valid model settings.", 400)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		dbError(w, err)
		return
	}
	defer tx.Rollback()
	item := agent{toroid.NewSessionID(), req.Name, req.Settings}
	raw, _ := json.Marshal(item.Settings)
	res, err := tx.ExecContext(r.Context(), `INSERT INTO webui_agents(id,name,config) VALUES(?,?,?) ON CONFLICT(name) DO NOTHING`, item.ID, item.Name, string(raw))
	if err != nil {
		dbError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "An agent already has that name.", 409)
		return
	}
	if req.Preview != "" {
		_, err = tx.ExecContext(r.Context(), `UPDATE webui_chats SET agent=?, draft=NULL WHERE trace_id=? AND draft IS NOT NULL`, item.ID, req.Preview)
		if err != nil {
			dbError(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		dbError(w, err)
		return
	}
	reply(w, item)
}
