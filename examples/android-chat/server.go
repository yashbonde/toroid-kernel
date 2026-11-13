package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

const (
	tokenCookie = "toroid_token"
	keyFileName = "openrouter.key"
	maxSessions = 8 // chats kept in memory; the oldest idle one is evicted
)

// chatSystemPrompt replaces the kernel's software-engineering prompt. It is
// the whole system prompt and the cached prefix of every request, so it stays
// byte-stable: anything that changes (the date, the open-to-do count) rides on
// each user message as a system reminder instead, see turnReminder. Every
// system token is paid on every message, so it is short.
const chatSystemPrompt = `You are a helpful, concise assistant in a chat app on the user's phone.

You can manage the user's to-do list. It lives in a calendar named "Toroid", so
every to-do also shows in the phone's Calendar app on its due date.
- When the user mentions a to-do, task or reminder, use the tools (tasks_list,
  task_add, task_update, task_done, task_delete). Never say you added, changed
  or deleted something unless a tool call in this reply did it.
- To change or delete a to-do, take its id from tasks_list or an earlier tool
  result. If several to-dos match, ask which one.
- Resolve relative dates ("today", "tomorrow", "Friday", "next week") against
  the date and time in the most recent <system-reminder> (it is refreshed
  after the chat sits quiet for a while, so its to-do count may be stale; use
  tasks_list for the current list).
- After a change, confirm what changed in one short line.
- If a tool fails, say plainly what failed.

You can also keep documents for the user. They are saved in the phone's Files
app under Documents › Toroid (files_list, file_save, file_read, file_open,
file_delete).
- Save a file only when the user explicitly asks to store or save something
  (in a doc, document, note or file). Never save on your own initiative, and
  never say you saved something unless file_save in this reply did it.
- If the user names no format, save a self-contained HTML document (.html): a
  <title>, a small inline <style> that reads well on a 360px-wide screen in
  light colours, real headings, lists and tables, and no external resources
  (no scripts, fonts, images or links to load). If the user names a format,
  save that: .txt for plain text, or .md, .json, .csv and so on.
- Give files short, clear names. Never replace an existing file unless the
  user asks; then pass overwrite.
- Open a file (file_open) only when the user asks to see or open it ("save and
  open" counts). HTML opens as a web page, images and video in Gallery, audio
  in Music, anything else as plain text.
- Delete a file only when the user explicitly asks.

You can also use any phone app for the user, but only through the app tools
(app_list, app_open, screen_look, tap, swipe, type, key, app_close), and only
on a background display the user cannot see. Every app is used this way; never
drive the user's own screen.
- Prefer the to-do and file tools above for to-dos, reminders and documents.
  Reach for an app only when the user asks to use or show a specific app, or
  when a task genuinely needs one (for example a calendar's own UI).
- For anything on the web (a site, news, the weather, a search), open it with
  app_open's url: the site itself (https://news.ycombinator.com) or a search
  (https://duckduckgo.com/?q=weather+in+pune). Never type into the browser's
  address bar. Then read the page with screen_look, and swipe to see more.
- Only browse and read: never download, install or set anything (a file, an
  app, a wallpaper) unless the user asked for exactly that.
- app_open puts the app on a background display and makes it active. Then
  screen_look to see it. You never see the screenshot: a separate vision model
  looks at it and screen_look returns its description, which lists each
  visible control and text with its centre x,y in display pixels. Plan from
  that description and tap at the coordinates it gives.
- Pass screen_look a question whenever you need something specific ("where is
  the start date field?", "is the title filled in?"). If the description is
  unclear or a target is missing, look again with a sharper question rather
  than guessing a tap.
- Look before every tap, and look again after to confirm what happened.
- To go back or dismiss something, prefer the app's own on-screen back arrow,
  X or Cancel. key back is only sent when it is safe, and may be refused.
- Never type a password or payment details into an app; stop and ask the user
  to do that part themselves.
- Ask the user first before anything destructive or hard to undo in an app:
  deleting, sending, posting, buying or paying.
- Stop as soon as you have what the user asked for: app_close, then answer.
- Report only what screen_look showed you in this reply. If you could not get
  it, say so in a line; never describe steps you did not take.

Answer directly. Prefer short paragraphs and short lists; use Markdown sparingly
(bold, inline code, fenced code blocks). The screen is narrow, so avoid wide
tables. If you are unsure, say so.`

// chatSystemPromptNoDelegate is chatSystemPrompt for a chat with the vision
// delegate turned off in settings: screen_look then shows the chat model the
// screenshot itself (on a vision model) or only a text description.
var chatSystemPromptNoDelegate = strings.Replace(chatSystemPrompt, appsLookRules, appsLookRulesNoDelegate, 1)

// appsLookRules is the part of chatSystemPrompt about screen_look with the
// vision delegate on; appsLookRulesNoDelegate replaces it when it is off.
const appsLookRules = `- app_open puts the app on a background display and makes it active. Then
  screen_look to see it. You never see the screenshot: a separate vision model
  looks at it and screen_look returns its description, which lists each
  visible control and text with its centre x,y in display pixels. Plan from
  that description and tap at the coordinates it gives.
- Pass screen_look a question whenever you need something specific ("where is
  the start date field?", "is the title filled in?"). If the description is
  unclear or a target is missing, look again with a sharper question rather
  than guessing a tap.
- Look before every tap, and look again after to confirm what happened.
`

const appsLookRulesNoDelegate = `- app_open puts the app on a background display and makes it active. Then
  screen_look to see it: it returns what is on screen, and a screenshot on a
  vision model. Look before every tap, and look again after to confirm what
  happened. Coordinates are display pixels; the screenshot carries a
  labelled grid (x,y every 100 px) to read them from.
`

type server struct {
	dataDir  string
	workDir  string
	token    string
	spendCap float64
	catalog  *catalog
	sessions *sessionStore
	chats    *chatStore
	tools    *toolHost   // the phone's to-do and file store; tests point it at a fake host
	apps     *appsBridge // the phone's app bridge (@toroid-system) for app-driving tools
	settings *settingsStore
	keyMu    sync.Mutex

	// agents.db, opened on the first "@" model (agents.go).
	agentsOnce sync.Once
	agents     *agentStore
	agentsErr  error
}

func newServer(dataDir, token string, spendCap float64) *server {
	return &server{
		dataDir:  dataDir,
		workDir:  filepath.Join(dataDir, "work"),
		token:    token,
		spendCap: spendCap,
		catalog:  newCatalog(),
		sessions: newSessionStore(),
		chats:    newChatStore(filepath.Join(dataDir, "chats")),
		tools:    newToolHost(toolSocket),
		apps:     newAppsBridge(systemSocket),
		settings: newSettingsStore(dataDir),
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(web)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Laptop convenience: /?token=... sets the cookie and drops the query.
		// The Android app sets the cookie through CookieManager instead.
		if t := r.URL.Query().Get("token"); t != "" && s.tokenOK(t) {
			setTokenCookie(w, t)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/models", s.auth(s.handleModels))
	mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /api/key", s.auth(s.handleKey))
	mux.HandleFunc("GET /api/settings", s.auth(s.handleGetSettings))
	mux.HandleFunc("POST /api/settings", s.auth(s.handlePostSettings))
	mux.HandleFunc("POST /api/new", s.auth(s.handleNew))
	mux.HandleFunc("POST /api/chat", s.auth(s.handleChat))
	mux.HandleFunc("GET /api/display/{id}/frame.jpg", s.auth(s.handleDisplayFrame))
	mux.HandleFunc("GET /api/history", s.auth(s.handleHistory))
	mux.HandleFunc("GET /api/chats/{id}", s.auth(s.handleGetChat))
	mux.HandleFunc("GET /api/chats/{id}/live", s.auth(s.handleLive))
	mux.HandleFunc("POST /api/chats/{id}/stop", s.auth(s.handleStop))
	mux.HandleFunc("DELETE /api/chats/{id}", s.auth(s.handleDeleteChat))
	mux.HandleFunc("POST /api/palette", s.auth(s.handlePalette))
	mux.HandleFunc("GET /api/image", s.auth(s.handleImageInfo))
	return loopbackHostOnly(mux)
}

// loopbackHostOnly rejects requests whose Host is not a loopback name, which
// stops a web page from reaching this server through DNS rebinding.
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

// auth requires the session token. Other apps on the phone can reach loopback
// too; without the token they cannot spend the user's credit.
func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if c, err := r.Cookie(tokenCookie); err == nil && t == "" {
			t = c.Value
		}
		if t == "" || !s.tokenOK(t) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or bad token"})
			return
		}
		h(w, r)
	}
}

func setTokenCookie(w http.ResponseWriter, t string) {
	http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.catalog.list()
	def := ""
	if len(models) > 0 {
		def = models[0].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "tiers": tiers, "default": def, "spend_cap_usd": s.spendCap})
}

// readKey returns the OpenRouter key: the key file first, then the env.
func (s *server) readKey() (key, source string) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if b, err := os.ReadFile(filepath.Join(s.dataDir, keyFileName)); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, "file"
		}
	}
	if k := strings.TrimSpace(os.Getenv(toroid.GatewayKeyEnv)); k != "" {
		return k, "env"
	}
	return "", ""
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	key, source := s.readKey()
	// Never echo the key, not even a fragment of it.
	writeJSON(w, http.StatusOK, map[string]any{"has_key": key != "", "key_source": source})
}

func (s *server) handleKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" || strings.ContainsAny(key, " \r\n\t") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key must be a single non-empty token"})
		return
	}
	s.keyMu.Lock()
	err := writeFileAtomic(filepath.Join(s.dataDir, keyFileName), []byte(key), 0o600)
	s.keyMu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save key"})
		return
	}
	// Chats built with the old key would keep using it; start them over.
	s.sessions.closeAll()
	log.Printf("api key replaced")
	writeJSON(w, http.StatusOK, map[string]any{"has_key": true, "key_source": "file"})
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *server) handleNew(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	if body.Session != "" {
		s.sessions.remove(body.Session)
	}
	writeJSON(w, http.StatusOK, map[string]string{"session": toroid.NewSessionID()})
}

// newKernel builds a chat kernel on model whose only tools are the to-do and
// file tools, seeded with history and allowed to spend at most budget USD.
func (s *server) newKernel(ctx context.Context, model string, history []llm.Message, budget float64) (*toroid.Kernel, error) {
	key, _ := s.readKey()
	if key == "" {
		return nil, errors.New("no OpenRouter key: add one in Settings")
	}
	return s.newKernelWithKey(ctx, model, history, budget, key)
}

func (s *server) newKernelWithKey(ctx context.Context, model string, history []llm.Message, budget float64, key string) (*toroid.Kernel, error) {
	if isSetupModel(model) {
		return s.newSetupKernel(ctx, model, history, budget, key)
	}
	if err := os.MkdirAll(s.workDir, 0o700); err != nil {
		return nil, err
	}
	noSkills := false
	// Whether this model accepts image input, from OpenRouter's catalog. It
	// decides both whether screen_look attaches a screenshot and whether the
	// kernel keeps tool-result media (ImageInputOverride below), so the two
	// never disagree for a model the kernel's built-in table does not know.
	imageCapable := s.catalog.visionFor(model)
	// With the vision delegate on (the default), screen_look always attaches
	// the gridded screenshot, and the kernel hands it to the vision model: the
	// chat model, vision-capable or not, only ever reads its description.
	var vision *toroid.VisionDelegate
	look, prefix := lookText, chatSystemPromptNoDelegate
	if imageCapable {
		look = lookImage
	}
	if vm := s.settings.get().visionModel(); vm != "" {
		// Minimal reasoning effort: reading a screen needs no long think, and
		// Jev Router, if chosen, otherwise picks a frontier model for each
		// screenshot at several times the cost of a reading that is just as
		// usable.
		vision = &toroid.VisionDelegate{Model: vm, Prompt: screenVisionPrompt, MaxTokens: 1200, ReasoningEffort: "minimal"}
		look, prefix = lookDelegate, chatSystemPrompt
	}
	reg := agentTools(s.tools)
	registerAppsTools(reg, s.apps, look)
	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model: model,
		// Compaction and subagents default to a model the gateway may not
		// carry; keep everything on the model the user picked.
		SmallerModel: model,
		APIKey:       key,
		WorkDir:      s.workDir,
		Thinking:     toroid.ThinkingNone,
		// The to-do, file and app tools only: no shell, no direct file access,
		// skills or delegation, which also keeps every request small.
		Tools:                reg,
		IncludeComputerTools: false,
		IncludeSubagentTools: false,
		LoadSkills:           &noSkills,
		// Send screenshots from screen_look to a vision model (and keep them off
		// a text-only one), using OpenRouter's modalities rather than the
		// kernel's built-in table, which does not know these gateway ids.
		ImageInputOverride: &imageCapable,
		Vision:             vision,
		// Driving an app is look-tap-look-tap: many small steps. The repeat-call
		// guard still stops a genuine spin early.
		MaxIter: 20,
		// A saved HTML document is written as one tool call, so leave room.
		MaxTokens: 8192,
		// Compact long chats early so one message never re-sends a huge history.
		TotalContextSize:      32000,
		CompactionBufferSize:  8000,
		MaxTranscriptSpendUSD: budget,
	})
	if err != nil {
		return nil, err
	}
	k.SystemPromptPrefix = prefix
	k.SystemPrompt = "" // the changing facts go on each message: turnReminder
	k.History = history
	s.hookKernel(k, model)
	return k, nil
}

// hookKernel wires a kernel's events to the reply's stream: tool calls, who
// is working (model is the LLM it runs on), guard stops and vision costs.
func (s *server) hookKernel(k *toroid.Kernel, model string) {
	// Tool calls show in the reply as they happen. The hooks find the reply's
	// stream in the context the kernel fires them with.
	k.On(toroid.EventPreToolUse, func(ctx context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			if cs := streamFrom(ctx); cs != nil {
				cs.toolStarted(p.CallID, p.Name)
			}
		}
		return nil
	})
	// Who is working right now, for the reply's activity line: the chat model
	// deciding the next step, the vision model reading a screenshot, or a tool.
	k.On(toroid.EventTurnStarted, func(ctx context.Context, e toroid.Event) error {
		if cs := streamFrom(ctx); cs != nil {
			cs.activity("llm", model, "")
		}
		return nil
	})
	k.On(toroid.EventVisionStarted, func(ctx context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.VisionPayload); ok {
			if cs := streamFrom(ctx); cs != nil {
				cs.activity("vision", p.Model, p.Name)
			}
		}
		return nil
	})
	finished := func(ctx context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			if cs := streamFrom(ctx); cs != nil {
				cs.toolFinished(p.CallID, p.Name, p.Result, p.Error)
			}
		}
		return nil
	}
	k.On(toroid.EventPostToolUse, finished)
	k.On(toroid.EventPostToolUseFailure, finished)
	// The step cap and the repeat-call guard end a reply silently, which leaves
	// the user with whatever the model said before its last tool call ("Let me
	// look again."). Say plainly that it stopped unfinished.
	k.On(toroid.EventGuardTriggered, func(ctx context.Context, e toroid.Event) error {
		p, ok := e.Payload.(*toroid.GuardTriggeredPayload)
		cs := streamFrom(ctx)
		if !ok || cs == nil {
			return nil
		}
		if note := guardNote(p.Name); note != "" {
			_, _ = cs.Write([]byte(note)) // after a tool call, so it starts a new paragraph
		}
		return nil
	})
	// Each vision call is already in the kernel's cost; this keeps the split
	// for the reply's done event and the log.
	k.On(toroid.EventVisionDescribed, func(ctx context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.VisionPayload); ok {
			if m := visionMeterFrom(ctx); m != nil {
				m.add(p.CostUSD)
			}
			if p.Error != "" {
				log.Printf("vision %s for %s failed: %s", strings.TrimPrefix(p.Model, gatewayPrefix), p.Name, p.Error)
			} else {
				log.Printf("vision %s for %s: in=%d out=%d reasoning=%d cost=$%.6f", strings.TrimPrefix(p.Model, gatewayPrefix), p.Name,
					p.Usage.Input+p.Usage.CacheRead+p.Usage.CacheWrite, p.Usage.Output, p.Usage.Reasoning, p.CostUSD)
			}
		}
		return nil
	})
}

// guardNote is the line added to a reply that a kernel guard ended before the
// model answered, or "" for guards that only nudge the model.
func guardNote(name string) string {
	switch name {
	case "max_iter":
		return "_I stopped here: this took more steps than one reply allows, and I had not finished. Say \"continue\" and I will pick up where I left off._"
	case "loop_guard":
		return "_I stopped here: I was repeating the same step without getting anywhere. Tell me how you would like to proceed._"
	}
	return ""
}

// visionMeter sums one reply's vision-delegate calls and cost.
type visionMeter struct {
	mu    sync.Mutex
	calls int
	cost  float64
}

func (m *visionMeter) add(cost float64) {
	m.mu.Lock()
	m.calls++
	m.cost += cost
	m.mu.Unlock()
}

func (m *visionMeter) totals() (int, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, m.cost
}

type visionMeterKey struct{}

func withVisionMeter(ctx context.Context, m *visionMeter) context.Context {
	return context.WithValue(ctx, visionMeterKey{}, m)
}

func visionMeterFrom(ctx context.Context) *visionMeter {
	m, _ := ctx.Value(visionMeterKey{}).(*visionMeter)
	return m
}

// reminderGap is how long a chat must sit quiet before its next message
// carries a fresh system reminder. Within it, the reminder already in the
// history is close enough, and a message without one keeps the request a
// plain append to what the prompt cache already holds.
const reminderGap = 6 * time.Hour

// needsReminder reports whether the message sent at now gets a system
// reminder: the chat's first message here (including the first after a
// restart, whose rebuilt history has none), or the first after a gap of more
// than reminderGap. It records now as the last message. Callers hold c.mu.
func (c *chatSession) needsReminder(now time.Time) bool {
	need := c.asked.IsZero() || now.Sub(c.asked) > reminderGap
	c.asked = now
	return need
}

// turnReminder is the system reminder put in front of a user message (see
// needsReminder for which ones): the
// local date, time and zone that relative dates resolve against, and the
// number of open to-dos (left out when the phone cannot answer). It goes on the
// message rather than in the system prompt so that nothing before the newest
// message ever changes: the system prompt and every earlier turn, reminders
// included, stay byte-identical and keep hitting the prompt cache.
func (s *server) turnReminder(ctx context.Context, now time.Time) string {
	zone := os.Getenv("TZ") // set by ChatService to the phone's zone id
	if zone == "" {
		zone = now.Format("MST")
	}
	p := "Now: " + now.Format("Monday, 2 January 2006, 15:04") + " (" + zone + ", UTC" + now.Format("-07:00") + "). " +
		"Today is " + now.Format(time.DateOnly) + "."
	if n, ok := s.tools.openCount(ctx); ok {
		p += fmt.Sprintf("\nOpen to-dos: %d.", n)
	}
	return "<system-reminder>\n" + p + "\n</system-reminder>"
}

type chatRequest struct {
	Session string `json:"session"`
	Model   string `json:"model"`
	Message string `json:"message"`
}

// doneEvent is the final event of a reply.
type doneEvent struct {
	Model          string  `json:"model"`
	ReplyCostUSD   float64 `json:"reply_cost_usd"`
	SessionCostUSD float64 `json:"session_cost_usd"`
	SpendCapUSD    float64 `json:"spend_cap_usd"`
	CapReached     bool    `json:"cap_reached,omitempty"`
	// VisionCostUSD is the part of ReplyCostUSD spent by the vision delegate
	// on VisionCalls screenshots.
	VisionCostUSD float64 `json:"vision_cost_usd,omitempty"`
	VisionCalls   int     `json:"vision_calls,omitempty"`
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty message"})
		return
	}
	if !s.knownModel(req.Model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown model"})
		return
	}
	if req.Session == "" {
		req.Session = toroid.NewSessionID()
	} else if !validChatID(req.Session) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad session id"})
		return
	}
	sess := s.sessions.get(req.Session)
	if !sess.mu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a reply is already streaming in this chat"})
		return
	}
	sess.touch()
	if err := s.loadRecord(sess, req.Session); err != nil {
		sess.mu.Unlock()
		log.Printf("chat %s: load: %v", req.Session, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load this chat"})
		return
	}

	// The reply runs on its own goroutine and context (see turn.go), holding
	// sess.mu until it ends; this request only follows it. When the app goes
	// to the background and this stream drops, the reply carries on.
	ctx, cancel := context.WithCancel(context.Background())
	t := newTurn(cancel)
	_ = t.event("session", map[string]string{"session": req.Session})
	// The message itself, so a client that opens the chat mid-reply (it is
	// only saved when the reply ends) can show what is being answered.
	_ = t.event("prompt", map[string]string{"text": req.Message})
	sess.setLive(t)
	go s.runTurn(ctx, sess, req, t)
	t.follow(r.Context(), startSSE(w))
}

// runTurn generates one reply into t. It owns sess.mu, taken by handleChat,
// and releases it when the reply is saved.
func (s *server) runTurn(ctx context.Context, sess *chatSession, req chatRequest, t *turn) {
	// From here on the message belongs to the chat: it is saved with whatever
	// reply it got, even none (an error, the cap, or the user pressing Stop).
	cs := &chatStream{sse: t}
	asked := time.Now()
	before := sess.spent()
	defer func() {
		if p := recover(); p != nil { // a request handler's panic was recovered by net/http; this goroutine's is not
			log.Printf("chat %s: panic: %v", req.Session, p)
			_ = t.event("error", map[string]string{"error": "internal error"})
		}
		s.saveTurn(sess, req, asked, cs.replyText(), cs.toolLines(), sess.spent()-before)
		sess.setLive(nil)
		t.finish()
		t.cancel()
		sess.touch()
		sess.mu.Unlock()
	}()

	meter := &visionMeter{}
	ctx = withChatID(withVisionMeter(withChatStream(ctx, cs), meter), req.Session)
	if err := s.ensureKernel(ctx, sess, req.Model, t); err != nil {
		_ = t.event("error", map[string]string{"error": err.Error()})
		return
	}
	k := sess.kernel
	spendCap := s.capFor(req.Model)
	if before >= spendCap {
		_ = t.event("error", map[string]string{"error": fmt.Sprintf("this chat reached its $%.2f spend cap; start a new chat", spendCap)})
		return
	}

	start := time.Now()
	// The kernel's history keeps the reminder with the message, so earlier
	// turns stay identical; the saved chat keeps only what the user typed. An
	// agent setup ("@" model) gets no reminder: it has no to-dos or dates.
	msg := req.Message
	if !isSetupModel(req.Model) && sess.needsReminder(start) {
		msg = s.turnReminder(ctx, start) + "\n\n" + msg
	}
	err := k.Stream(ctx, msg, cs, toroid.WithReasoningStream(cs.reasoning()))
	firstToken := "-"
	if t := cs.firstTokenAt(); !t.IsZero() {
		firstToken = t.Sub(start).Round(time.Millisecond).String()
	}

	after := sess.spent()
	visionCalls, visionCost := meter.totals()
	done := doneEvent{
		Model:          req.Model,
		ReplyCostUSD:   after - before,
		SessionCostUSD: after,
		SpendCapUSD:    spendCap,
		CapReached:     after >= spendCap,
		VisionCostUSD:  roundUSD(visionCost),
		VisionCalls:    visionCalls,
	}
	log.Printf("chat %s: model=%s first_token=%s total=%s cost=$%.6f (vision $%.6f over %d looks) session=$%.6f",
		req.Session, strings.TrimPrefix(req.Model, gatewayPrefix), firstToken,
		time.Since(start).Round(time.Millisecond), done.ReplyCostUSD, visionCost, visionCalls, done.SessionCostUSD)
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("chat %s: stopped: %v", req.Session, err)
			_ = t.event("stopped", done)
			return
		}
		log.Printf("chat %s: %v", req.Session, err)
		_ = t.event("error", map[string]string{"error": err.Error()})
		_ = t.event("done", done)
		return
	}
	if !cs.sentText() {
		msg := "the model returned no text"
		if done.CapReached {
			msg = fmt.Sprintf("this chat reached its $%.2f spend cap; start a new chat", spendCap)
		}
		_ = t.event("error", map[string]string{"error": msg})
	}
	_ = t.event("done", done)
}

// handleDisplayFrame streams the latest JPEG of a background display for the
// live view. Authenticated like every other /api call, so another app on
// loopback cannot pull the frames. It only serves the display that is actually
// active, and never display 0 (the user's screen).
func (s *server) handleDisplayFrame(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.Error(w, "bad display id", http.StatusBadRequest)
		return
	}
	active, _, _, ok := s.apps.activeDisplay()
	if !ok || id != active {
		http.Error(w, "display not active", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), toolTimeout)
	defer cancel()
	jpeg, err := s.apps.frameJPEG(ctx, id)
	if err != nil {
		http.Error(w, "no frame", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(jpeg)
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	chats, err := s.chats.list()
	if err != nil {
		log.Printf("history: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read history"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

func (s *server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validChatID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad chat id"})
		return
	}
	rec, err := s.chats.load(id)
	if errors.Is(err, fs.ErrNotExist) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such chat"})
		return
	}
	if err != nil {
		log.Printf("chat %s: load: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load this chat"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validChatID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad chat id"})
		return
	}
	// A streaming reply would save the chat again when it finishes.
	if !s.sessions.removeIdle(id) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a reply is streaming in this chat"})
		return
	}
	if err := s.chats.remove(id); errors.Is(err, fs.ErrNotExist) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such chat"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete this chat"})
		return
	}
	log.Printf("chat %s: deleted", id)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// loadRecord gives sess its saved chat: the one on disk when the chat is not
// in memory (a past chat opened after a restart, or one evicted), else a new,
// empty record. A loaded chat's spend still counts against its cap. Callers
// hold sess.mu.
func (s *server) loadRecord(sess *chatSession, id string) error {
	if sess.record != nil {
		return nil
	}
	rec, err := s.chats.load(id)
	if errors.Is(err, fs.ErrNotExist) {
		sess.record = &chatRecord{ID: id}
		return nil
	}
	if err != nil {
		return err
	}
	sess.record = rec
	if sess.kernel == nil {
		sess.retired = rec.CostUSD
	}
	return nil
}

// saveTurn appends the user's message and the reply that reached them
// (possibly partial, possibly none) to the chat, and writes it. The reply keeps
// a one-line summary of each tool call it made.
func (s *server) saveTurn(sess *chatSession, req chatRequest, asked time.Time, reply string, tools []string, cost float64) {
	rec := sess.record
	rec.add(chatMessage{Role: "user", Text: req.Message, At: stamp(asked)})
	if reply != "" || len(tools) > 0 {
		rec.add(chatMessage{Role: "assistant", Text: reply, Tools: tools, Model: req.Model, At: stamp(time.Now()), CostUSD: roundUSD(cost)})
	}
	rec.Model, rec.CostUSD = req.Model, roundUSD(sess.spent())
	if err := s.chats.save(rec); err != nil {
		log.Printf("chat %s: save: %v", rec.ID, err)
	}
}

// roundUSD drops float noise (a reply's cost is a difference of two sums)
// below a billionth of a cent.
func roundUSD(v float64) float64 { return math.Round(v*1e12) / 1e12 }

// ensureKernel gives sess a kernel on model. Switching models keeps the
// conversation: the new kernel starts from the old one's history, and the
// money already spent still counts against this chat's cap. A chat's first
// kernel starts from its saved turns, if any.
//
// A change of vision delegate in settings rebuilds the kernel the same way, on
// the chat's next message.
func (s *server) ensureKernel(ctx context.Context, sess *chatSession, model string, sse eventSink) error {
	// A setup's kernel uses the same vision model, for image_look.
	vision := s.settings.get().visionModel()
	if sess.kernel != nil && sess.model == model && kernelVision(sess.kernel) == vision {
		return nil
	}
	var history []llm.Message
	if sess.kernel == nil && sess.record != nil {
		history = sess.record.history()
	}
	if sess.kernel != nil {
		history = append([]llm.Message(nil), sess.kernel.History...)
		sess.retired += sess.kernel.RunningCostUSD()
		_ = sess.kernel.Close()
		sess.kernel = nil
	}
	spendCap := s.capFor(model)
	budget := spendCap - sess.retired
	if budget <= 0 {
		return fmt.Errorf("this chat reached its $%.2f spend cap; start a new chat", spendCap)
	}
	k, err := s.newKernel(ctx, model, history, budget)
	if err != nil {
		return err
	}
	if sess.model != "" && sess.model != model {
		_ = sse.event("notice", map[string]string{"switched_to": model})
	}
	sess.kernel, sess.model = k, model
	return nil
}

// kernelVision is the vision delegate a kernel was built with, or "" for none.
func kernelVision(k *toroid.Kernel) string {
	if k.Cfg.Vision == nil {
		return ""
	}
	return k.Cfg.Vision.Model
}

// chatSession is one conversation. mu is held for the whole of a reply, so a
// chat streams one answer at a time.
type chatSession struct {
	mu      sync.Mutex
	kernel  *toroid.Kernel
	model   string
	retired float64     // spend of kernels replaced by model switches, or of a resumed chat
	record  *chatRecord // the chat as saved; nil until its first message here
	asked   time.Time   // when this kernel last got a message; zero before the first

	lastMu   sync.Mutex
	lastUsed time.Time
	live     *turn // the reply running now, or nil; guarded by lastMu
}

func (c *chatSession) setLive(t *turn) {
	c.lastMu.Lock()
	c.live = t
	c.lastMu.Unlock()
}

func (c *chatSession) liveTurn() *turn {
	c.lastMu.Lock()
	defer c.lastMu.Unlock()
	return c.live
}

func (c *chatSession) touch() {
	c.lastMu.Lock()
	c.lastUsed = time.Now()
	c.lastMu.Unlock()
}

func (c *chatSession) idleSince() time.Time {
	c.lastMu.Lock()
	defer c.lastMu.Unlock()
	return c.lastUsed
}

// spent is the chat's total LLM spend. Callers hold c.mu.
func (c *chatSession) spent() float64 {
	total := c.retired
	if c.kernel != nil {
		total += c.kernel.RunningCostUSD()
	}
	return total
}

func (c *chatSession) close() {
	if c.kernel != nil {
		_ = c.kernel.Close()
	}
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]*chatSession
}

func newSessionStore() *sessionStore { return &sessionStore{m: map[string]*chatSession{}} }

// get returns the session for id, creating it (and evicting the least recently
// used idle chat past maxSessions) when it does not exist yet.
func (st *sessionStore) get(id string) *chatSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	if c, ok := st.m[id]; ok {
		return c
	}
	if len(st.m) >= maxSessions {
		var oldID string
		var old *chatSession
		for sid, c := range st.m {
			if old == nil || c.idleSince().Before(old.idleSince()) {
				oldID, old = sid, c
			}
		}
		if old != nil && old.mu.TryLock() {
			old.close()
			old.mu.Unlock()
			delete(st.m, oldID)
		}
	}
	c := &chatSession{lastUsed: time.Now()}
	st.m[id] = c
	return c
}

// lookup returns the session for id without creating one.
func (st *sessionStore) lookup(id string) (*chatSession, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.m[id]
	return c, ok
}

func (st *sessionStore) remove(id string) {
	st.mu.Lock()
	c, ok := st.m[id]
	delete(st.m, id)
	st.mu.Unlock()
	if ok && c.mu.TryLock() {
		c.close()
		c.mu.Unlock()
	}
}

// removeIdle drops id from memory unless a reply is streaming in it, which it
// reports as false.
func (st *sessionStore) removeIdle(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.m[id]
	if !ok {
		return true
	}
	if !c.mu.TryLock() {
		return false
	}
	defer c.mu.Unlock()
	c.close()
	delete(st.m, id)
	return true
}

func (st *sessionStore) closeAll() {
	st.mu.Lock()
	all := st.m
	st.m = map[string]*chatSession{}
	st.mu.Unlock()
	for _, c := range all {
		if c.mu.TryLock() {
			c.close()
			c.mu.Unlock()
		}
	}
}
