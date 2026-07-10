package tools

import (
	"context"

	"github.com/yashbonde/toroid-kernel/llm"
)

type ToolDef struct {
	Name        string
	Description string // short (discovery) — shown to LLM in tool schema
	Template    string // name of the .tool.tmpl file (full documentation)
	Handler     llm.ToolHandler
}

type Agent interface {
	WorkDir() string
	SessionID() string
	Fire(ctx context.Context, kind string, payload any) error
	RunSubagent(ctx context.Context, task string) (string, error)
}

type Registry struct {
	tools map[string]*ToolDef
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*ToolDef)}
}

func (r *Registry) Register(t *ToolDef) {
	r.tools[t.Name] = t
}

func (r *Registry) Lookup(name string) (*ToolDef, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Tools() map[string]*ToolDef {
	return r.tools
}

// Execute runs a registered tool by name with raw JSON arguments.
func (r *Registry) Execute(ctx context.Context, name string, argumentsJSON string) (llm.ToolResult, error) {
	t, ok := r.tools[name]
	if !ok || t.Handler == nil {
		return llm.NewErrorResult("tool not found: " + name), nil
	}
	return t.Handler.Run(ctx, argumentsJSON)
}
