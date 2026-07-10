// Package llm is toroid's own minimal model interface: a pi-style "one LLM call
// = one step" layer that speaks OpenAI-compatible chat completions only (via a
// LiteLLM gateway). It has no dependency on any third-party model SDK — the data
// model, wire client, tool abstraction, and JSON-schema generation are all
// in-repo. See assets/llm-step-port-scope.md.
package llm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Role is a chat message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Part is one piece of a message's content. Concrete parts: TextPart, FilePart,
// ToolCallPart, ToolResultPart, ReasoningPart.
type Part interface{ partKind() string }

// TextPart is plain text content.
type TextPart struct{ Text string }

func (TextPart) partKind() string { return "text" }

// FilePart is inline media — an image or a PDF (base64 is built from Data at
// wire time). Images become image_url data URIs; PDFs become file blocks.
type FilePart struct {
	Filename  string // optional; used for PDF file blocks
	MediaType string // e.g. "image/png", "application/pdf"
	Data      []byte
}

func (FilePart) partKind() string { return "file" }

// ToolCallPart is the model asking for a tool to run. Arguments is the raw JSON
// string emitted by the model.
type ToolCallPart struct {
	ID        string
	Name      string
	Arguments string
}

func (ToolCallPart) partKind() string { return "tool_call" }

// ToolResultPart carries the result of a tool the kernel executed, keyed back to
// the call by ToolCallID. Files carries optional media the tool returned (e.g. a
// screenshot the read tool loaded) — sent as content blocks alongside the text.
type ToolResultPart struct {
	ToolCallID string
	Content    string
	IsError    bool
	Files      []FilePart
}

func (ToolResultPart) partKind() string { return "tool_result" }

// ReasoningPart is model thinking/reasoning content.
type ReasoningPart struct{ Text string }

func (ReasoningPart) partKind() string { return "reasoning" }

// Message is one turn in a conversation: a role plus ordered content parts.
type Message struct {
	Role  Role
	Parts []Part
}

// NewUserMessage builds a user text message.
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Parts: []Part{TextPart{Text: text}}}
}

// NewSystemMessage builds a system text message.
func NewSystemMessage(text string) Message {
	return Message{Role: RoleSystem, Parts: []Part{TextPart{Text: text}}}
}

// NewAssistantText builds an assistant text message.
func NewAssistantText(text string) Message {
	return Message{Role: RoleAssistant, Parts: []Part{TextPart{Text: text}}}
}

// Text returns the concatenated text of all TextParts in the message.
func (m Message) Text() string { return TextOf(m.Parts) }

// ToolCalls returns every ToolCallPart in the message.
func (m Message) ToolCalls() []ToolCallPart { return ToolCallsOf(m.Parts) }

// TextOf returns the concatenated text across a slice of parts.
func TextOf(parts []Part) string {
	var s string
	for _, p := range parts {
		if tp, ok := p.(TextPart); ok {
			s += tp.Text
		}
	}
	return s
}

// ToolCallsOf returns every ToolCallPart across a slice of parts.
func ToolCallsOf(parts []Part) []ToolCallPart {
	var out []ToolCallPart
	for _, p := range parts {
		if tc, ok := p.(ToolCallPart); ok {
			out = append(out, tc)
		}
	}
	return out
}

// --- JSON round-trip ---
//
// The kernel persists history as JSON (EventAssistantTurn payloads replayed on
// resume), so Message must survive marshal → unmarshal. Parts serialize with a
// "kind" discriminator matching each part's partKind().

// wirePart is the serialized form of any Part.
type wirePart struct {
	Kind string `json:"kind"`
	// text / reasoning
	Text string `json:"text,omitempty"`
	// file
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"` // base64
	// tool_call
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// tool_result
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Content    string     `json:"content,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
	Files      []wirePart `json:"files,omitempty"` // nested file parts
}

func partToWire(p Part) wirePart {
	switch v := p.(type) {
	case TextPart:
		return wirePart{Kind: "text", Text: v.Text}
	case ReasoningPart:
		return wirePart{Kind: "reasoning", Text: v.Text}
	case FilePart:
		return wirePart{Kind: "file", Filename: v.Filename, MediaType: v.MediaType, Data: base64.StdEncoding.EncodeToString(v.Data)}
	case ToolCallPart:
		return wirePart{Kind: "tool_call", ID: v.ID, Name: v.Name, Arguments: v.Arguments}
	case ToolResultPart:
		w := wirePart{Kind: "tool_result", ToolCallID: v.ToolCallID, Content: v.Content, IsError: v.IsError}
		for _, f := range v.Files {
			w.Files = append(w.Files, partToWire(f))
		}
		return w
	default:
		return wirePart{Kind: "text"}
	}
}

func wireToPart(w wirePart) (Part, error) {
	switch w.Kind {
	case "text":
		return TextPart{Text: w.Text}, nil
	case "reasoning":
		return ReasoningPart{Text: w.Text}, nil
	case "file":
		data, err := base64.StdEncoding.DecodeString(w.Data)
		if err != nil {
			return nil, fmt.Errorf("file part data: %w", err)
		}
		return FilePart{Filename: w.Filename, MediaType: w.MediaType, Data: data}, nil
	case "tool_call":
		return ToolCallPart{ID: w.ID, Name: w.Name, Arguments: w.Arguments}, nil
	case "tool_result":
		tr := ToolResultPart{ToolCallID: w.ToolCallID, Content: w.Content, IsError: w.IsError}
		for _, fw := range w.Files {
			fp, err := wireToPart(fw)
			if err != nil {
				return nil, err
			}
			if f, ok := fp.(FilePart); ok {
				tr.Files = append(tr.Files, f)
			}
		}
		return tr, nil
	default:
		return nil, fmt.Errorf("unknown part kind %q", w.Kind)
	}
}

type wireMsg struct {
	Role  Role       `json:"role"`
	Parts []wirePart `json:"parts"`
}

// MarshalJSON serializes the message with a kind-discriminated part array.
func (m Message) MarshalJSON() ([]byte, error) {
	w := wireMsg{Role: m.Role, Parts: make([]wirePart, 0, len(m.Parts))}
	for _, p := range m.Parts {
		w.Parts = append(w.Parts, partToWire(p))
	}
	return json.Marshal(w)
}

// UnmarshalJSON restores a message serialized by MarshalJSON. Unknown part kinds
// are skipped (forward compatibility) rather than failing the whole message.
func (m *Message) UnmarshalJSON(b []byte) error {
	var w wireMsg
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Role = w.Role
	m.Parts = m.Parts[:0]
	for _, wp := range w.Parts {
		p, err := wireToPart(wp)
		if err != nil {
			continue
		}
		m.Parts = append(m.Parts, p)
	}
	return nil
}
