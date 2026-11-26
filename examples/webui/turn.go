package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

type commentAnchor struct {
	Kind  string `json:"kind"`
	Index int    `json:"index"`
	Start int    `json:"start"`
}

type comment struct {
	ID     string         `json:"id,omitempty"`
	Anchor *commentAnchor `json:"anchor,omitempty"`
	Quote  string         `json:"quote"`
	Note   string         `json:"note"`
}

type turnRequest struct {
	Model       string    `json:"model"`
	Workspace   string    `json:"workspace"`
	Chat        string    `json:"chat"`
	Agent       string    `json:"agent"`
	Draft       *settings `json:"draft"`
	Text        string    `json:"text"`
	Comments    []comment `json:"comments"`
	Attachments []string  `json:"attachments"`
}

func (req turnRequest) prompt() (string, bool) {
	if len(req.Text) > 64000 || len(req.Comments) > 40 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(req.Text))
	for _, c := range req.Comments {
		if len(c.ID) > 64 {
			return "", false
		}
		if a := c.Anchor; a != nil {
			if a.Index < 0 || a.Start < 0 {
				return "", false
			}
			switch a.Kind {
			case "assistant", "user", "arguments", "output":
			default:
				return "", false
			}
		}
		q, n := strings.TrimSpace(c.Quote), strings.TrimSpace(c.Note)
		if q == "" || n == "" || len(q) > 16000 || len(n) > 8000 {
			return "", false
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("> " + strings.ReplaceAll(q, "\n", "\n> ") + "\n" + n)
	}
	return b.String(), b.Len() > 0 || len(req.Attachments) > 0
}

func (a *app) turn(w http.ResponseWriter, r *http.Request) {
	var req turnRequest
	if !decode(w, r, &req) {
		return
	}
	prompt, ok := req.prompt()
	if !ok {
		http.Error(w, "Enter a message or a comment within the size limit.", 400)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	id := req.Chat
	resume := id != ""
	if resume {
		if _, _, err := a.chatProfile(ctx, id); err != nil {
			dbError(w, err)
			return
		}
	} else {
		id = toroid.NewSessionID()
	}
	if _, busy := a.active.LoadOrStore(id, (*liveReply)(nil)); busy {
		http.Error(w, "A reply is already streaming in this chat.", 409)
		return
	}
	launched := false
	defer func() {
		if !launched {
			a.active.Delete(id)
		}
	}()

	s, err := a.profile(ctx, req.Agent)
	if err != nil {
		dbError(w, err)
		return
	}
	var draft any
	if req.Draft != nil {
		s = *req.Draft
		if !s.valid() {
			http.Error(w, "Check the draft's model and instructions.", 400)
			return
		}
		raw, _ := json.Marshal(s)
		draft = string(raw)
	}
	o, err := a.options(r.WithContext(ctx), id)
	if err != nil {
		dbError(w, err)
		return
	}
	if resume && req.Workspace != "" && req.Workspace != o.Workspace {
		http.Error(w, "The workspace is fixed after the first message.", 409)
		return
	}
	if !resume && req.Workspace != "" {
		o.Workspace, err = directory(req.Workspace)
		if err != nil {
			http.Error(w, "Choose an existing folder.", 400)
			return
		}
		o.CWD = o.Workspace
	}
	if _, err = directory(o.CWD); err != nil {
		http.Error(w, "The workspace folder is unavailable.", 409)
		return
	}
	if req.Model != "" {
		s.Model = req.Model
		if !s.valid() {
			http.Error(w, "Choose a valid model.", 400)
			return
		}
	}
	o.Model = req.Model
	items, err := inspectAttachments(req.Attachments)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, item := range items {
		if item.Kind == "image" && !toroid.ResolveModel(s.Model).SupportsImage() {
			http.Error(w, "Choose an image-capable model in the composer to send this image.", http.StatusBadRequest)
			return
		}
	}
	// Credentials follow the server's provider configuration. A custom model
	// cannot carry the base model's explicit credential across providers.
	token := ""
	if strings.HasPrefix(s.Model, "openai/") == strings.HasPrefix(a.base.Settings.Model, "openai/") {
		token = os.Getenv("TOROID_LLM_TOKEN")
	}
	if strings.HasPrefix(s.Model, "openai/") && token == "" {
		token = os.Getenv(toroid.OpenAIKeyEnv)
		if token == "" {
			http.Error(w, "Set OPENAI_API_KEY on the server to use an OpenAI model.", http.StatusBadRequest)
			return
		}
	}
	k, err := a.newKernel(ctx, toroid.Config{
		Model:                s.Model,
		ModelMetadataWait:    100 * time.Millisecond,
		APIKey:               token,
		Thinking:             s.Thinking,
		WorkDir:              o.CWD,
		MaxIter:              a.maxIter,
		SessionID:            id,
		Save:                 true,
		Resume:               resume,
		IncludeComputerTools: s.Computer,
		IncludeSubagentTools: s.Delegate,
		LoadSkills:           &s.Skills,
		DisableBackground:    true,
	})
	if err != nil {
		http.Error(w, "Could not open the kernel. Check the provider and local store.", 500)
		return
	}
	defer func() {
		if !launched {
			k.Close()
		}
	}()
	if strings.TrimSpace(s.System) != "" {
		k.SystemPrompt += "\n\n" + s.System
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		dbError(w, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO webui_chats(trace_id,agent,draft) VALUES(?,?,?)
		ON CONFLICT(trace_id) DO UPDATE SET agent=excluded.agent,draft=excluded.draft`, id, req.Agent, draft)
	if err != nil {
		dbError(w, err)
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO webui_session_options(trace_id,workspace,cwd,model) VALUES(?,?,?,?)
		ON CONFLICT(trace_id) DO UPDATE SET model=excluded.model`, id, o.Workspace, o.CWD, o.Model)
	if err != nil {
		dbError(w, err)
		return
	}
	if req.Agent != "base" {
		var present int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM webui_agents WHERE id=?`, req.Agent).Scan(&present); err != nil {
			dbError(w, err)
			return
		}
	}
	if !resume {
		title := []rune(strings.Join(strings.Fields(prompt), " "))
		if len(title) == 0 && len(items) > 0 {
			title = []rune(filepath.Base(items[0].Path))
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO webui_created_chats(trace_id) VALUES(?)`, id); err != nil {
			dbError(w, err)
			return
		}
		if _, err = tx.ExecContext(ctx, `UPDATE traces SET title=? WHERE trace_id=?`, string(title[:min(72, len(title))]), id); err != nil {
			dbError(w, err)
			return
		}
	}

	if err = tx.Commit(); err != nil {
		dbError(w, err)
		return
	}
	var before int64
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM events WHERE trace_id=? AND span_id=?`, id, id).Scan(&before); err != nil {
		dbError(w, err)
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &liveReply{id: toroid.NewSessionID(), before: before, cancel: cancel, changed: make(chan struct{})}
	launched = true
	a.active.Store(id, run)
	go func() {
		defer k.Close()
		defer cancel()
		frame := a.generate(runCtx, k, id, prompt, items, req.Comments, run.publish)
		a.active.Delete(id)
		run.finish(frame)
	}()
	w.Header().Set("X-Toroid-Chat", id)
	w.Header().Set("X-Toroid-Stream", run.id)
	run.serve(w, r)
}

func (a *app) generate(ctx context.Context, k *toroid.Kernel, id, prompt string, items []toroid.Attachment, comments []comment, send func(any) error) any {
	var stepStarted int64
	sendUsage := func(ctx context.Context, _ toroid.Event) error {
		u, err := a.spend(ctx, id)
		if err != nil {
			return err
		}
		used, window := k.ContextUsage()
		u.Window = window
		u.Model = k.Cfg.Model
		if used > 0 {
			u.Used = &used
		}
		return send(map[string]any{"type": "usage", "usage": u})
	}
	k.On(toroid.EventPostCompact, sendUsage)
	k.On(toroid.EventTurnCompleted, sendUsage)
	k.On(toroid.EventTurnFailed, sendUsage)
	k.On(toroid.EventTurnStarted, func(_ context.Context, e toroid.Event) error {
		stepStarted = e.EmitTS / int64(time.Millisecond)
		return nil
	})
	k.On(toroid.EventUserPromptSubmit, func(ctx context.Context, e toroid.Event) error {
		if len(comments) > 0 {
			notes, err := json.Marshal(comments)
			if err != nil {
				return err
			}
			if _, err = a.db.ExecContext(ctx, `INSERT INTO webui_comments(trace_id,prompt_seq,notes) VALUES(?,?,?)`, id, e.Seq, string(notes)); err != nil {
				return err
			}
		}
		if err := send(map[string]any{"type": "accepted", "timestamp": e.EmitTS / int64(time.Millisecond), "chat": id, "text": prompt, "comments": comments, "attachments": items}); err != nil {
			return err
		}
		return sendUsage(ctx, e)
	})
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		p := e.Payload.(*toroid.ToolUsePayload)
		return send(map[string]any{"type": "tool", "timestamp": e.EmitTS / int64(time.Millisecond), "call": toolCall{ID: p.CallID, Name: p.Name, Arguments: p.Args}})
	})
	for _, event := range []toroid.EventKind{toroid.EventPostToolUse, toroid.EventPostToolUseFailure} {
		k.On(event, func(_ context.Context, e toroid.Event) error {
			p := e.Payload.(*toroid.ToolUseResultPayload)
			output := p.Result
			if output == "" {
				output = p.Error
			}
			return send(map[string]any{"type": "tool_result", "id": p.CallID, "output": output, "failed": e.Kind == toroid.EventPostToolUseFailure})
		})
	}
	err := k.Stream(ctx, prompt, io.Discard, toroid.WithAttachments(items), toroid.WithTextDeltas(func(turn, text string) error {
		return send(map[string]any{"type": "delta", "timestamp": stepStarted, "turn": turn, "text": text})
	}))
	if err != nil {
		if ctx.Err() != nil {
			return map[string]string{"type": "error", "text": "Reply stopped."}
		}
		return map[string]string{"type": "error", "text": fmt.Sprintf("Reply interrupted. Check the connection and provider configuration for %s.", k.Cfg.Model)}
	}
	var ready int64
	if err := a.db.QueryRowContext(ctx, `INSERT INTO webui_replies(trace_id,ready) VALUES(?,1) ON CONFLICT(trace_id) DO UPDATE SET ready=ready+1 RETURNING ready`, id).Scan(&ready); err != nil {
		return map[string]string{"type": "error", "text": "Reply saved. Could not update its read status."}
	}
	return map[string]any{"type": "done", "ready": ready}
}
