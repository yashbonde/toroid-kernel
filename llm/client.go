package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Chat is one model wire: a single completion or a single stream — one call ==
// one product llm-step. Implementations: Client (OpenAI-compatible chat
// completions: LiteLLM gateway, OpenAI direct) and AnthropicClient (native
// Anthropic messages API).
type Chat interface {
	Complete(ctx context.Context, req Request) (*Response, error)
	StreamComplete(ctx context.Context, req Request) (*Stream, error)
}

// Client speaks OpenAI-compatible chat completions (LiteLLM gateway or OpenAI
// direct). One Complete/Stream call == one product llm-step.
type Client struct {
	BaseURL   string // includes the /v1 segment
	APIKey    string
	UserAgent string
	HTTP      *http.Client
}

// NewClient builds a gateway client. baseURL must include /v1.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:    apiKey,
		UserAgent: "toroid-kernel",
		HTTP:      &http.Client{},
	}
}

// ResponseFormat requests structured output: the model must emit a JSON object
// matching Schema. Sent as OpenAI response_format type json_schema, which
// LiteLLM translates for upstreams that use a different mechanism.
type ResponseFormat struct {
	Name        string
	Description string
	Schema      map[string]any
}

// RequestMetadata identifies the product hierarchy for one outbound LLM call.
// TraceID is the caller-requested wire name for the individual call; it is
// distinct from the kernel/OTEL TraceID, which identifies a transcript graph.
type RequestMetadata struct {
	TranscriptID string `json:"transcript_id"`
	ChatID       string `json:"chat_id"`
	TurnID       string `json:"turn_id"`
	TraceID      string `json:"trace_id"`
}

// Request is one llm-step request. System is sent as a single leading system
// message (never duplicated in Messages) for a stable cache prefix.
type Request struct {
	Model           string
	SystemPrefix    string
	System          string
	Messages        []Message
	Tools           []Tool
	Metadata        RequestMetadata
	ToolChoice      string // "", "auto", "none", "required", or a tool name to force
	MaxTokens       *int
	Temperature     *float64
	ResponseFormat  *ResponseFormat // structured output (one object llm-step)
	ReasoningEffort string          // "", "low", "medium", "high" — gateway maps to upstream thinking budgets
	// CachePrompt requests prompt-cache breakpoints (Anthropic-style ephemeral
	// cache_control, which LiteLLM forwards): one on the system message and one
	// on the last user/tool message, so each turn re-reads the previous turns at
	// cache-read price instead of full input price. Set for model families that
	// need explicit breakpoints (Claude); OpenAI routes auto-cache and are left alone.
	CachePrompt bool
}

// Response is one non-streaming llm-step result.
type Response struct {
	Content      []Part
	FinishReason FinishReason
	Usage        Usage
	CallID       string // x-litellm-call-id
}

// Text returns the assistant text of the response.
func (r *Response) Text() string { return TextOf(r.Content) }

// ToolCalls returns the tool calls in the response.
func (r *Response) ToolCalls() []ToolCallPart { return ToolCallsOf(r.Content) }

// Complete performs one non-streaming chat completion.
func (c *Client) Complete(ctx context.Context, req Request) (*Response, error) {
	body, err := c.buildBody(req, false)
	if err != nil {
		return nil, err
	}
	httpResp, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway %d: %s", httpResp.StatusCode, snippet(data))
	}

	var wire chatResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode response: %w (%s)", err, snippet(data))
	}
	resp := &Response{
		Usage:  wire.Usage.toUsage(),
		CallID: httpResp.Header.Get("x-litellm-call-id"),
	}
	if len(wire.Choices) > 0 {
		ch := wire.Choices[0]
		resp.Content = ch.Message.toParts()
		resp.FinishReason = normalizeFinishReason(ch.FinishReason)
	}
	return resp, nil
}

// Delta is one incremental chunk of a streaming llm-step.
type Delta struct {
	Text      string // assistant text delta
	Reasoning string // reasoning/thinking delta
}

// Stream is a live streaming llm-step. Range over Deltas until it closes, then
// call Final for the assembled Response. If the context is cancelled mid-stream
// the partial content is preserved and Final returns the context error.
type Stream struct {
	Deltas <-chan Delta
	// Final blocks until the stream is drained and returns the assembled
	// response plus any terminal error. Safe to call after Deltas closes.
	Final func() (*Response, error)
}

// StreamComplete performs one streaming chat completion over SSE. Usage arrives
// in the final chunk (stream_options.include_usage).
func (c *Client) StreamComplete(ctx context.Context, req Request) (*Stream, error) {
	body, err := c.buildBody(req, true)
	if err != nil {
		return nil, err
	}
	httpResp, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		data, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("gateway %d: %s", httpResp.StatusCode, snippet(data))
	}

	deltas := make(chan Delta)
	done := make(chan struct{})
	var final Response
	var finalErr error

	go func() {
		defer close(done)
		defer close(deltas)
		defer httpResp.Body.Close()

		var text, reasoning string
		toolCalls := map[int]*wireToolCall{} // accumulated by index
		finish := ""
		var usage wireUsage
		aborted := false

		sc := bufio.NewScanner(httpResp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			select {
			case <-ctx.Done():
				aborted = true
			default:
			}
			if aborted {
				break
			}
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var chunk chatChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue // tolerate malformed keep-alives
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			ch := chunk.Choices[0]
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			d := ch.Delta
			if d.Content != "" {
				text += d.Content
				select {
				case deltas <- Delta{Text: d.Content}:
				case <-ctx.Done():
					aborted = true
				}
			}
			if d.ReasoningContent != "" {
				reasoning += d.ReasoningContent
				select {
				case deltas <- Delta{Reasoning: d.ReasoningContent}:
				case <-ctx.Done():
					aborted = true
				}
			}
			for _, tc := range d.ToolCalls {
				acc, ok := toolCalls[tc.Index]
				if !ok {
					cp := tc
					toolCalls[tc.Index] = &cp
					continue
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Function.Name = tc.Function.Name
				}
				acc.Function.Arguments += tc.Function.Arguments
			}
			if aborted {
				break
			}
		}
		if err := sc.Err(); err != nil && finalErr == nil && !aborted {
			finalErr = err
		}

		var parts []Part
		if reasoning != "" {
			parts = append(parts, ReasoningPart{Text: reasoning})
		}
		if text != "" {
			parts = append(parts, TextPart{Text: text})
		}
		idxs := make([]int, 0, len(toolCalls))
		for i := range toolCalls {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			tc := toolCalls[i]
			parts = append(parts, ToolCallPart{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}

		final = Response{
			Content:      parts,
			FinishReason: normalizeFinishReason(finish),
			Usage:        usage.toUsage(),
			CallID:       httpResp.Header.Get("x-litellm-call-id"),
		}
		if aborted && finalErr == nil {
			finalErr = ctx.Err()
		}
	}()

	return &Stream{
		Deltas: deltas,
		Final: func() (*Response, error) {
			<-done
			return &final, finalErr
		},
	}, nil
}

// do issues the POST with auth, user-agent, and gateway trace/session headers.
func (c *Client) do(ctx context.Context, body []byte) (*http.Response, error) {
	return doWithRetry(ctx, c.HTTP, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")
		if c.UserAgent != "" {
			httpReq.Header.Set("User-Agent", c.UserAgent)
		}
		// Group a chat's turns under one upstream trace when a trace id is in context.
		if tid := sessionTraceID(ctx); tid != "" {
			httpReq.Header.Set("traceparent", "00-"+tid+"-"+randomHex(8)+"-01")
			httpReq.Header.Set("x-litellm-session-id", tid)
		}
		return httpReq, nil
	})
}

// doWithRetry sends a request built by build, retrying transient failures —
// network errors, 429, and 5xx — up to three times with exponential backoff,
// since a mid-chat 502 would otherwise kill a whole agent run.
func doWithRetry(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		var httpReq *http.Request
		httpReq, err = build()
		if err != nil {
			return nil, err
		}
		resp, err = client.Do(httpReq)
		retryable := err != nil || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt >= 3 || ctx.Err() != nil {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second): // 1s, 2s, 4s
		}
	}
}

// buildBody marshals a Request into the OpenAI chat-completions JSON body.
func (c *Client) buildBody(req Request, stream bool) ([]byte, error) {
	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if req.SystemPrefix != "" || req.System != "" {
		if req.CachePrompt {
			var blocks []map[string]any
			if req.SystemPrefix != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": req.SystemPrefix, "cache_control": map[string]any{"type": "ephemeral"}})
			}
			if req.System != "" {
				block := map[string]any{"type": "text", "text": req.System}
				if req.SystemPrefix == "" {
					block["cache_control"] = map[string]any{"type": "ephemeral"}
				}
				blocks = append(blocks, block)
			}
			msgs = append(msgs, wireMessage{Role: string(RoleSystem), Content: blocks})
		} else {
			system := strings.TrimSpace(req.SystemPrefix + "\n\n" + req.System)
			msgs = append(msgs, wireMessage{Role: string(RoleSystem), Content: system})
		}
	}
	for _, m := range req.Messages {
		msgs = append(msgs, toWireMessages(m)...)
	}
	if req.CachePrompt {
		// Second breakpoint on the last user/tool message (in the agent loop the
		// request always ends with one), so the whole conversation up to and
		// including it is served from cache on the next turn.
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != string(RoleUser) && msgs[i].Role != string(RoleTool) {
				continue
			}
			cc := map[string]any{"type": "ephemeral"}
			switch content := msgs[i].Content.(type) {
			case string:
				if content == "" {
					continue // an empty text block would be rejected upstream
				}
				msgs[i].Content = []map[string]any{{"type": "text", "text": content, "cache_control": cc}}
			case []map[string]any:
				if len(content) == 0 {
					continue
				}
				content[len(content)-1]["cache_control"] = cc
			}
			break
		}
	}

	model := strings.TrimPrefix(req.Model, "llmgateway/")
	model = strings.TrimPrefix(model, "openai/")
	body := map[string]any{
		"model":    model,
		"messages": msgs,
		"metadata": req.Metadata,
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		body["tools"] = tools
	}
	if req.ToolChoice != "" {
		switch req.ToolChoice {
		case "auto", "none", "required":
			body["tool_choice"] = req.ToolChoice
		default: // force a specific tool by name
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": req.ToolChoice}}
		}
	}
	if req.ResponseFormat != nil {
		js := map[string]any{
			"name":   req.ResponseFormat.Name,
			"schema": req.ResponseFormat.Schema,
			"strict": true,
		}
		if req.ResponseFormat.Description != "" {
			js["description"] = req.ResponseFormat.Description
		}
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": js}
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(body)
}

// --- wire types (OpenAI chat completions) ---

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // string or []contentBlock or nil
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toWireMessages converts one llm.Message into one or more OpenAI wire messages
// (a tool-role message with several results expands into one wire message per
// result, as the OpenAI protocol requires).
func toWireMessages(m Message) []wireMessage {
	switch m.Role {
	case RoleTool:
		var out []wireMessage
		for _, p := range m.Parts {
			if tr, ok := p.(ToolResultPart); ok {
				content := tr.Content
				if tr.IsError && !strings.HasPrefix(content, "Error:") {
					content = "Error: " + content
				}
				wm := wireMessage{Role: string(RoleTool), ToolCallID: tr.ToolCallID}
				if len(tr.Files) > 0 {
					// Tool result with media: send content blocks (text + files) so
					// vision-capable upstreams can see e.g. a screenshot the tool read.
					parts := []Part{TextPart{Text: content}}
					for _, f := range tr.Files {
						parts = append(parts, f)
					}
					wm.Content = toContentBlocks(parts)
				} else {
					wm.Content = content
				}
				out = append(out, wm)
			}
		}
		return out
	case RoleAssistant:
		wm := wireMessage{Role: string(RoleAssistant)}
		text := TextOf(m.Parts)
		for _, p := range m.Parts {
			if tc, ok := p.(ToolCallPart); ok {
				var w wireToolCall
				w.ID = tc.ID
				w.Type = "function"
				w.Function.Name = tc.Name
				w.Function.Arguments = tc.Arguments
				wm.ToolCalls = append(wm.ToolCalls, w)
			}
		}
		if text != "" {
			wm.Content = text
		} else if len(wm.ToolCalls) == 0 {
			wm.Content = ""
		}
		return []wireMessage{wm}
	default: // user / system
		for _, p := range m.Parts {
			if _, ok := p.(FilePart); ok {
				return []wireMessage{{Role: string(m.Role), Content: toContentBlocks(m.Parts)}}
			}
		}
		return []wireMessage{{Role: string(m.Role), Content: TextOf(m.Parts)}}
	}
}

// toContentBlocks builds the multimodal content array: text blocks, image_url
// data URIs for images, and file blocks (file_data data URIs) for PDFs.
func toContentBlocks(parts []Part) []map[string]any {
	var blocks []map[string]any
	for _, p := range parts {
		switch v := p.(type) {
		case TextPart:
			if v.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": v.Text})
			}
		case FilePart:
			uri := "data:" + v.MediaType + ";base64," + base64.StdEncoding.EncodeToString(v.Data)
			if strings.HasPrefix(v.MediaType, "image/") {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}})
			} else {
				file := map[string]any{"file_data": uri}
				if v.Filename != "" {
					file["filename"] = v.Filename
				}
				blocks = append(blocks, map[string]any{"type": "file", "file": file})
			}
		}
	}
	return blocks
}

type chatResponse struct {
	Choices []struct {
		FinishReason string      `json:"finish_reason"`
		Message      wireRespMsg `json:"message"`
	} `json:"choices"`
	Usage wireUsage `json:"usage"`
}

// chatChunk is one SSE chunk of a streaming response.
type chatChunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

type wireRespMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

func (m wireRespMsg) toParts() []Part {
	var parts []Part
	if m.ReasoningContent != "" {
		parts = append(parts, ReasoningPart{Text: m.ReasoningContent})
	}
	if m.Content != "" {
		parts = append(parts, TextPart{Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		parts = append(parts, ToolCallPart{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return parts
}

type wireUsage struct {
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	TotalTokens             int64 `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	PromptTokensDetails struct {
		CachedTokens        int64 `json:"cached_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_tokens"`
	} `json:"prompt_tokens_details"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// toUsage maps wire usage into llm.Usage, splitting cached tokens out of the
// prompt so Input holds only fresh (uncached) prompt tokens — matching the
// per-token pricing model where input / cache-read / cache-write are disjoint.
func (u wireUsage) toUsage() Usage {
	cacheRead := u.CacheReadInputTokens
	if cacheRead == 0 {
		cacheRead = u.PromptTokensDetails.CachedTokens
	}
	cacheWrite := u.CacheCreationInputTokens
	if cacheWrite == 0 {
		cacheWrite = u.PromptTokensDetails.CacheCreationTokens
	}
	input := u.PromptTokens - cacheRead - cacheWrite
	if input < 0 {
		input = 0
	}
	return Usage{
		Input:      input,
		Output:     u.CompletionTokens,
		Reasoning:  u.CompletionTokensDetails.ReasoningTokens,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// --- gateway trace context ---

type traceKey struct{}

// WithSessionTrace attaches a stable 32-hex trace id to ctx for one chat, so the
// client groups all of the chat's llm-steps under one upstream trace. Preserved
// if already present (e.g. a subagent within a parent chat).
func WithSessionTrace(ctx context.Context) context.Context {
	if v, _ := ctx.Value(traceKey{}).(string); v != "" {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, randomHex(16))
}

func sessionTraceID(ctx context.Context) string {
	v, _ := ctx.Value(traceKey{}).(string)
	return v
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
