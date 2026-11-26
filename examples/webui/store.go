package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

// Only these tables belong to this example. Messages stay in Toroid events.
const webSchema = `
CREATE TABLE IF NOT EXISTS webui_browser_sessions (
  digest BLOB PRIMARY KEY,
  host TEXT NOT NULL,
  expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS webui_agents (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	config TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS webui_chats (
	trace_id TEXT PRIMARY KEY REFERENCES traces(trace_id),
	pinned INTEGER NOT NULL DEFAULT 0,
	agent TEXT NOT NULL DEFAULT 'base',
	draft TEXT
);
CREATE TABLE IF NOT EXISTS webui_created_chats (
	trace_id TEXT PRIMARY KEY REFERENCES traces(trace_id)
);
CREATE TABLE IF NOT EXISTS webui_comments (
 trace_id TEXT NOT NULL REFERENCES traces(trace_id),
 prompt_seq INTEGER NOT NULL,
 notes TEXT NOT NULL,
 PRIMARY KEY(trace_id,prompt_seq)
);
CREATE TABLE IF NOT EXISTS webui_replies (
 trace_id TEXT PRIMARY KEY REFERENCES traces(trace_id),
 ready INTEGER NOT NULL,
 seen INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS webui_session_options (
	trace_id TEXT PRIMARY KEY REFERENCES traces(trace_id),
	workspace TEXT NOT NULL,
	cwd TEXT NOT NULL,
	model TEXT NOT NULL
);`

type settings struct {
	Model    string          `json:"model"`
	System   string          `json:"system"`
	Thinking toroid.Thinking `json:"thinking"`
	Computer bool            `json:"computer"`
	Skills   bool            `json:"skills"`
	Delegate bool            `json:"delegate"`
}

func (s settings) valid() bool {
	return strings.TrimSpace(s.Model) != "" && len(s.Model) <= 200 &&
		!strings.ContainsAny(s.Model, " \t\r\n") && len(s.System) <= 32000 &&
		(s.Thinking == toroid.ThinkingNone || s.Thinking == toroid.ThinkingLow || s.Thinking == toroid.ThinkingHigh)
}

type agent struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Settings settings `json:"settings"`
}

type chat struct {
	Ready     int64  `json:"ready,omitempty"`
	Seen      int64  `json:"seen,omitempty"`
	Stream    string `json:"stream,omitempty"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Pinned    bool   `json:"pinned"`
	Updated   int64  `json:"updated"` // milliseconds for browser Date
	Agent     string `json:"agent"`
	Workspace string `json:"workspace"`
}

const roots = ` FROM traces t JOIN spans s ON s.trace_id=t.trace_id AND s.span_id=t.trace_id
	AND s.parent_span_id='' LEFT JOIN webui_chats w ON w.trace_id=t.trace_id `

// Use the root's most recent event, including an interrupted prompt, for recency.
func (a *app) chats(ctx context.Context) ([]chat, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, title, pinned, activity, agent, workspace,ready,seen FROM (
		SELECT COALESCE(reply.ready,0) AS ready,COALESCE(reply.seen,0) AS seen,t.trace_id AS id, COALESCE(NULLIF(t.title,''), NULLIF(substr((
			SELECT json_extract(e.data,'$.payload.prompt') FROM events e WHERE e.trace_id=t.trace_id
			AND e.span_id=s.span_id AND e.kind='UserPromptSubmit' ORDER BY seq LIMIT 1),1,80),''),'Untitled chat') AS title,
		COALESCE(w.pinned,0) AS pinned, COALESCE(w.agent,'base') AS agent, COALESCE(opt.workspace,'') AS workspace,
		MAX(t.started_at, t.ended_at, COALESCE((SELECT e.ts FROM events e WHERE e.trace_id=t.trace_id
			AND e.span_id=s.span_id ORDER BY e.ts DESC LIMIT 1),0)) AS activity`+roots+
		`JOIN webui_created_chats origin ON origin.trace_id=t.trace_id LEFT JOIN webui_session_options opt ON opt.trace_id=t.trace_id LEFT JOIN webui_replies reply ON reply.trace_id=t.trace_id`+
		`) WHERE pinned=1 OR activity>=? ORDER BY pinned DESC, activity DESC, id`, time.Now().Add(-15*24*time.Hour).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []chat{}
	for rows.Next() {
		var c chat
		if err := rows.Scan(&c.ID, &c.Title, &c.Pinned, &c.Updated, &c.Agent, &c.Workspace, &c.Ready, &c.Seen); err != nil {
			return nil, err
		}
		if run := a.live(c.ID); run != nil {
			c.Stream = run.id
		}
		c.Updated /= int64(time.Millisecond)
		out = append(out, c)
	}
	return out, rows.Err()
}

type toolCall struct {
	startedAt int64   // first recorded event for this call, in milliseconds
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Arguments string  `json:"arguments"`
	Output    *string `json:"output,omitempty"`
	Failed    bool    `json:"failed,omitempty"`
}

type message struct {
	Comments  []comment `json:"comments,omitempty"`
	Timestamp int64     `json:"timestamp,omitempty"` // Unix milliseconds
	Point     string    `json:"point"`
	// Copy whole events through prefix, then the visible part of the current step.
	prefix     int64
	step       int
	toolResult *llm.ToolResultPart

	Role        string              `json:"role"`
	Text        string              `json:"text,omitempty"`
	Attachments []toroid.Attachment `json:"attachments,omitempty"`
	Call        *toolCall           `json:"call,omitempty"`
}

func (a *app) messages(ctx context.Context, id string) ([]message, error) {
	return readMessages(ctx, a.db, id, math.MaxInt64)
}

type transcriptReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readMessages(ctx context.Context, db transcriptReader, id string, through int64) ([]message, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.seq,e.ts,e.kind,json_extract(e.data,'$.payload'),c.notes FROM events e
  LEFT JOIN webui_comments c ON e.kind='UserPromptSubmit' AND c.trace_id=e.trace_id AND c.prompt_seq=e.seq
  WHERE e.trace_id=? AND e.span_id=? AND e.seq<=? AND kind IN ('UserPromptSubmit','TurnCompleted','AssistantTurn','TurnStarted','TurnFailed','PreToolUse','PostToolUse','PostToolUseFailure','PostCompact') ORDER BY seq`, id, id, through)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []message{}
	// A finished step stores both lifecycle events and structured messages.
	// Merge them by call ID, retaining the lifecycle events for interrupted
	// steps and the structured messages for older transcripts.
	var prefix, seq, timestamp, stepStarted int64
	step := 0
	results := map[string]*llm.ToolResultPart{}
	appendMessage := func(m message) {
		m.prefix, m.step = prefix, step
		m.Point = fmt.Sprintf("%d.%d.%s", prefix, len(out)-step, m.Role)
		out = append(out, m)
	}
	boundary := func(end int64) { prefix, step = end, len(out) }
	var batch []*toolCall
	byID := map[string]*toolCall{}
	callFor := func(id, name string) *toolCall {
		if call := byID[id]; call != nil {
			return call
		}
		call := &toolCall{ID: id, Name: name, startedAt: timestamp}
		byID[id] = call
		batch = append(batch, call)
		return call
	}
	flush := func() {
		for _, call := range batch {
			appendMessage(message{Role: "tool", Timestamp: call.startedAt, Call: call, toolResult: results[call.ID]})
		}
		batch = nil
		clear(byID)
		clear(results)
	}
	for rows.Next() {
		var kind string
		var raw, notes []byte
		if err := rows.Scan(&seq, &timestamp, &kind, &raw, &notes); err != nil {
			return nil, err
		}
		timestamp /= int64(time.Millisecond)
		switch kind {
		case "TurnStarted":
			flush()
			boundary(seq)
			stepStarted = timestamp
		case "PostCompact":
			stepStarted = 0
			flush()
			boundary(seq)
		case "PreToolUse":
			var p toroid.ToolUsePayload
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			callFor(p.CallID, p.Name).Arguments = p.Args
		case "PostToolUse", "PostToolUseFailure":
			var p toroid.ToolUseResultPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			call := callFor(p.CallID, p.Name)
			if p.Result == "" {
				p.Result = p.Error
			}
			call.Output = &p.Result
			call.Failed = kind == "PostToolUseFailure"
		case "UserPromptSubmit":
			stepStarted = 0
			flush()
			var p toroid.UserPromptPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			boundary(seq)
			m := message{Role: "user", Timestamp: timestamp, Text: p.Prompt, Attachments: p.Attachments}
			if len(notes) > 0 {
				if err := json.Unmarshal(notes, &m.Comments); err != nil {
					return nil, err
				}
			}
			appendMessage(m)
			boundary(seq)
		case "TurnFailed":
			stepStarted = 0
			flush()
			boundary(seq)
			appendMessage(message{Role: "status", Timestamp: timestamp, Text: "This turn was interrupted."})
			boundary(seq)
		default:
			var p struct {
				Content  []llm.Message
				Messages json.RawMessage
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			if kind == "AssistantTurn" && len(p.Messages) > 0 {
				if err := json.Unmarshal(p.Messages, &p.Content); err != nil {
					return nil, err
				}
			}
			for _, m := range p.Content {
				switch m.Role {
				case llm.RoleAssistant:
					if t := m.Text(); t != "" {
						at := stepStarted
						if at == 0 {
							at = timestamp
						}
						appendMessage(message{Role: "assistant", Timestamp: at, Text: t})
					}
					for _, tc := range m.ToolCalls() {
						call := callFor(tc.ID, tc.Name)
						call.Name, call.Arguments = tc.Name, tc.Arguments
					}
				case llm.RoleTool:
					for _, part := range m.Parts {
						if res, ok := part.(llm.ToolResultPart); ok {
							call := callFor(res.ToolCallID, "")
							call.Output, call.Failed = &res.Content, res.IsError
							results[res.ToolCallID] = &res
						}
					}
				}
			}
			if kind == "TurnCompleted" || kind == "AssistantTurn" {
				stepStarted = 0
				flush()
				if len(out) > step {
					// At the end of a completed step preserve its original structured payload.
					out[len(out)-1].prefix = seq
					out[len(out)-1].step = len(out)
				}
				boundary(seq)
			}
		}
	}
	flush()
	return out, rows.Err()
}

func (a *app) profile(ctx context.Context, id string) (settings, error) {
	if id == "base" {
		return a.base.Settings, nil
	}
	var raw []byte
	var s settings
	err := a.db.QueryRowContext(ctx, `SELECT config FROM webui_agents WHERE id=?`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &s)
	}
	return s, err
}

func (a *app) chatProfile(ctx context.Context, id string) (string, *settings, error) {
	var agentID string
	var raw sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT COALESCE(w.agent,'base'),w.draft`+roots+`WHERE t.trace_id=?`, id).Scan(&agentID, &raw)
	if err != nil || !raw.Valid {
		return agentID, nil, err
	}
	var draft settings
	err = json.Unmarshal([]byte(raw.String), &draft)
	return agentID, &draft, err
}
