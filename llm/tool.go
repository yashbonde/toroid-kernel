package llm

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
)

// ToolResult is what a tool returns to the model. Files carries optional media
// (e.g. an image the read tool loaded) sent as content blocks with the text.
type ToolResult struct {
	Content string // text shown to the model
	IsError bool   // true marks the result as an error
	Files   []FilePart
}

// NewTextResult builds a successful text tool result.
func NewTextResult(content string) ToolResult { return ToolResult{Content: content} }

// NewErrorResult builds an error tool result.
func NewErrorResult(content string) ToolResult { return ToolResult{Content: content, IsError: true} }

// ToolHandler is an executable tool the model can call. It replaces the
// third-party tool interface: name + description + a JSON-Schema parameter object
// + a Run that receives the raw argument JSON the model produced.
type ToolHandler interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON Schema (object) for the tool's arguments
	Run(ctx context.Context, argumentsJSON string) (ToolResult, error)
}

// Tool is the wire description of a tool sent to the model (no executor).
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolOf builds the wire Tool description from a handler.
func ToolOf(h ToolHandler) Tool {
	return Tool{Name: h.Name(), Description: h.Description(), Parameters: h.Parameters()}
}

// funcTool adapts a typed function into a ToolHandler, generating the parameter
// schema from the input type and unmarshaling arguments before each call.
type funcTool[T any] struct {
	name   string
	desc   string
	params map[string]any
	fn     func(ctx context.Context, input T) (ToolResult, error)
}

func (t *funcTool[T]) Name() string                  { return t.name }
func (t *funcTool[T]) Description() string            { return t.desc }
func (t *funcTool[T]) Parameters() map[string]any     { return t.params }

func (t *funcTool[T]) Run(ctx context.Context, argumentsJSON string) (ToolResult, error) {
	var input T
	if s := strings.TrimSpace(argumentsJSON); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &input); err != nil {
			return NewErrorResult("invalid tool arguments: " + err.Error()), nil
		}
	}
	return t.fn(ctx, input)
}

// NewTool builds a ToolHandler from a typed function. The JSON-Schema for the
// arguments is generated from T (see GenerateSchema).
func NewTool[T any](name, description string, fn func(ctx context.Context, input T) (ToolResult, error)) ToolHandler {
	var zero T
	return &funcTool[T]{
		name:   name,
		desc:   description,
		params: GenerateSchema(reflect.TypeOf(zero)),
		fn:     fn,
	}
}

// RawTool builds a ToolHandler from an explicit schema and a raw-JSON handler —
// for tools whose input schema is not a Go struct (e.g. MCP passthrough tools).
func RawTool(name, description string, params map[string]any, run func(ctx context.Context, argumentsJSON string) (ToolResult, error)) ToolHandler {
	return &rawTool{name: name, desc: description, params: params, run: run}
}

type rawTool struct {
	name   string
	desc   string
	params map[string]any
	run    func(ctx context.Context, argumentsJSON string) (ToolResult, error)
}

func (t *rawTool) Name() string              { return t.name }
func (t *rawTool) Description() string        { return t.desc }
func (t *rawTool) Parameters() map[string]any { return t.params }
func (t *rawTool) Run(ctx context.Context, argumentsJSON string) (ToolResult, error) {
	return t.run(ctx, argumentsJSON)
}
