package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

func (a *app) rename(w http.ResponseWriter, r *http.Request) {
	var req struct{ Title string }
	if !decode(w, r, &req) {
		return
	}
	title := strings.Join(strings.Fields(req.Title), " ")
	if title == "" || utf8.RuneCountInString(title) > 120 {
		http.Error(w, "Enter a title of up to 120 characters.", 400)
		return
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE traces SET title=? WHERE trace_id=? AND trace_id IN (SELECT trace_id FROM webui_created_chats)`, title, r.PathValue("id"))
	if err != nil {
		dbError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		dbError(w, sql.ErrNoRows)
		return
	}
	reply(w, map[string]string{"title": title})
}

// A boundary inside a parallel tool batch needs a valid assistant call and tool
// result for each included call. Later siblings must never enter the branch.
func partialStep(messages []message) []llm.Message {
	var content []llm.Message
	for _, m := range messages {
		switch m.Role {
		case "assistant":
			content = append(content, llm.NewAssistantText(m.Text))
		case "tool":
			c := m.Call
			result := m.toolResult
			if result == nil {
				result = &llm.ToolResultPart{ToolCallID: c.ID, Content: "No result was recorded before this fork.", IsError: true}
				if c.Output != nil {
					result.Content = *c.Output
					result.IsError = c.Failed
				}
			}
			content = append(content,
				llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.ToolCallPart{ID: c.ID, Name: c.Name, Arguments: c.Arguments}}},
				llm.Message{Role: llm.RoleTool, Parts: []llm.Part{*result}})
		}
	}
	return content
}

func (a *app) fork(w http.ResponseWriter, r *http.Request) {
	var req struct{ Point string }
	if !decode(w, r, &req) {
		return
	}
	ctx, source := r.Context(), r.PathValue("id")
	id, now := toroid.NewSessionID(), time.Now().UnixNano()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		dbError(w, err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO traces(trace_id,title,started_at,previous_trace_id)
		SELECT ?, 'Fork · ' || substr(COALESCE(NULLIF(t.title,''),'Conversation'),1,112), ?, t.trace_id
		FROM traces t JOIN webui_created_chats c USING(trace_id) WHERE t.trace_id=?`, id, now, source)
	if err != nil {
		dbError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		dbError(w, sql.ErrNoRows)
		return
	}
	messages, err := readMessages(ctx, tx, source, math.MaxInt64)
	if err != nil {
		dbError(w, err)
		return
	}
	index := -1
	for i, m := range messages {
		if m.Point == req.Point {
			index = i
			break
		}
	}
	if index < 0 {
		http.Error(w, "This message is unavailable. Reopen the conversation and try again.", 409)
		return
	}
	m := messages[index]
	if err = copyFork(ctx, tx, id, source, now, m.prefix); err != nil {
		dbError(w, err)
		return
	}
	if content := partialStep(messages[m.step : index+1]); len(content) > 0 {
		raw, err := json.Marshal(content)
		if err != nil {
			dbError(w, err)
			return
		}
		e := toroid.Event{TraceID: id, SessionID: id, SpanID: id, Kind: toroid.EventTurnCompleted, Seq: uint64(m.prefix + 1), EmitTS: now, Payload: toroid.TurnPayload{Content: raw}}
		data, err := json.Marshal(e)
		if err != nil {
			dbError(w, err)
			return
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(trace_id,span_id,ts,seq,kind,data) VALUES(?,?,?,?,?,?)`, id, id, now, e.Seq, e.Kind, string(data)); err != nil {
			dbError(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		dbError(w, err)
		return
	}
	reply(w, map[string]string{"id": id})
}

func copyFork(ctx context.Context, tx *sql.Tx, id, source string, now, prefix int64) error {
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO spans(trace_id,span_id,model,started_at) SELECT ?,?,model,? FROM spans WHERE trace_id=? AND span_id=?`, []any{id, id, now, source, source}},
		{`INSERT INTO webui_created_chats(trace_id) VALUES(?)`, []any{id}},
		{`INSERT INTO webui_chats(trace_id,agent,draft) SELECT ?,agent,draft FROM webui_chats WHERE trace_id=?`, []any{id, source}},
		{`INSERT INTO webui_session_options(trace_id,workspace,cwd,model) SELECT ?,workspace,cwd,model FROM webui_session_options WHERE trace_id=?`, []any{id, source}},
		{`INSERT INTO webui_comments(trace_id,prompt_seq,notes)
    SELECT ?,prompt_seq,notes FROM webui_comments WHERE trace_id=? AND prompt_seq<=?`, []any{id, source, prefix}},
		{`INSERT INTO events(trace_id,span_id,ts,seq,kind,data)
   SELECT ?,?,ts,seq,kind,json_set(data,'$.trace_id',?,'$.span_id',?,'$.session_id',?)
			FROM events WHERE trace_id=? AND span_id=? AND seq<=?`, []any{id, id, id, id, id, source, source, prefix}},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			return err
		}
	}
	return nil
}
