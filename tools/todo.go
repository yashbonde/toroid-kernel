package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

const createTodosTable = `
CREATE TABLE IF NOT EXISTS todos (
    session_id TEXT NOT NULL,
    id         TEXT NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'pending',
    details    TEXT NOT NULL DEFAULT '',
    position   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, id)
)`

// Task is the full task object used when creating a new task.
type Task struct {
	ID      string `json:"id"               jsonschema:"description=Unique task ID (short slug e.g. 'setup-db')"`
	Title   string `json:"title,omitempty"  jsonschema:"description=Task title (required when creating)"`
	Status  string `json:"status,omitempty" jsonschema:"description=pending | in_progress | completed | cancelled — omit to keep current"`
	Details string `json:"details,omitempty" jsonschema:"description=Optional extra detail — omit to keep current"`
}

type TodoWriteArgs struct {
	Tasks []Task `json:"tasks" jsonschema:"description=List of tasks to create or partially update. To update only status send just {id, status}. To create send {id, title, status}. Never resend fields that haven't changed."`
}

type TodoReadArgs struct{}

func ensureTodosTable(db *sql.DB) error {
	_, err := db.Exec(createTodosTable)
	return err
}

func NewTodoWriteTool(a Agent, db *sql.DB, desc string) *ToolDef {
	fTool := fantasy.NewAgentTool("todo_write", desc, func(ctx context.Context, args TodoWriteArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if db == nil {
			return fantasy.ToolResponse{Type: "text", Content: "todo storage unavailable"}, nil
		}
		if err := ensureTodosTable(db); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("todo table: %w", err)
		}
		sessionID := a.SessionID()

		// Load existing tasks to preserve order and merge
		rows, err := db.QueryContext(ctx,
			`SELECT id, title, status, details, position FROM todos WHERE session_id = ? ORDER BY position`,
			sessionID)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		existing := map[string]Task{}
		order := []string{}
		maxPos := 0
		for rows.Next() {
			var t Task
			var pos int
			if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.Details, &pos); err != nil {
				rows.Close()
				return fantasy.ToolResponse{}, err
			}
			existing[t.ID] = t
			order = append(order, t.ID)
			if pos > maxPos {
				maxPos = pos
			}
		}
		rows.Close()

		for _, tm := range args.Tasks {
			if tm.ID == "" {
				continue
			}
			cur, exists := existing[tm.ID]
			if !exists {
				maxPos++
				cur = Task{ID: tm.ID}
				order = append(order, tm.ID)
				_, err := db.ExecContext(ctx,
					`INSERT INTO todos (session_id, id, title, status, details, position) VALUES (?, ?, ?, ?, ?, ?)`,
					sessionID, cur.ID, "", "pending", "", maxPos)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
			}
			if tm.Title != "" {
				cur.Title = tm.Title
			}
			if tm.Status != "" {
				cur.Status = tm.Status
			}
			if tm.Details != "" {
				cur.Details = tm.Details
			}
			existing[tm.ID] = cur

			_, err := db.ExecContext(ctx,
				`UPDATE todos SET title = ?, status = ?, details = ? WHERE session_id = ? AND id = ?`,
				cur.Title, cur.Status, cur.Details, sessionID, cur.ID)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}

			if cur.Status == "completed" {
				_ = a.Fire(ctx, "TaskCompleted", map[string]any{
					"task_id": cur.ID,
					"title":   cur.Title,
					"status":  cur.Status,
				})
			}
		}

		return fantasy.ToolResponse{Type: "text", Content: "ok"}, nil
	})

	return &ToolDef{
		Name:        "todo_write",
		Description: desc,
		Template:    "todowrite.tool.tmpl",
		AgentTool:   fTool,
	}
}

func NewTodoReadTool(a Agent, db *sql.DB, desc string) *ToolDef {
	fTool := fantasy.NewAgentTool("todo_read", desc, func(ctx context.Context, args TodoReadArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if db == nil {
			return fantasy.ToolResponse{Type: "text", Content: "[]"}, nil
		}
		if err := ensureTodosTable(db); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("todo table: %w", err)
		}

		rows, err := db.QueryContext(ctx,
			`SELECT id, title, status, details FROM todos WHERE session_id = ? ORDER BY position`,
			a.SessionID())
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		defer rows.Close()

		var tasks []Task
		for rows.Next() {
			var t Task
			if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.Details); err != nil {
				return fantasy.ToolResponse{}, err
			}
			tasks = append(tasks, t)
		}
		if tasks == nil {
			tasks = []Task{}
		}
		b, _ := json.Marshal(tasks)
		return fantasy.ToolResponse{Type: "text", Content: string(b)}, nil
	})

	return &ToolDef{
		Name:        "todo_read",
		Description: desc,
		Template:    "todoread.tool.tmpl",
		AgentTool:   fTool,
	}
}
