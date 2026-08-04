package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// AnthropicClient speaks the native Anthropic messages API
// (POST {base}/messages). It implements the same Chat surface as the
// OpenAI-compatible Client, mapping the shared Request/Response types onto
// Anthropic's wire: system as a top-level block array, tool results inside
// user messages, tool_use blocks in assistant messages, and explicit
// cache_control breakpoints (Anthropic has no automatic prompt caching).
type AnthropicClient struct {
	BaseURL   string // includes the /v1 segment
	APIKey    string
	UserAgent string
	HTTP      *http.Client
}

var _ Chat = (*AnthropicClient)(nil)

// AnthropicBaseURL is the Anthropic API base, including the /v1 segment.
const AnthropicBaseURL = "https://api.anthropic.com/v1"

// NewAnthropicClient builds a native Anthropic messages client.
func NewAnthropicClient(apiKey string) *AnthropicClient {
	return &AnthropicClient{
		BaseURL:   AnthropicBaseURL,
		APIKey:    apiKey,
		UserAgent: "toroid-kernel",
		HTTP:      &http.Client{},
	}
}

// defaultMaxTokens is used when the caller sets no output budget: the messages
// API requires max_tokens on every request.
const defaultMaxTokens = 8192

// buildBody marshals a Request into an Anthropic messages JSON body.
func (c *AnthropicClient) buildBody(req Request, stream bool) ([]byte, error) {
	model := strings.TrimPrefix(req.Model, "anthropic/")

	body := map[string]any{
		"model":    model,
		"messages": anthropicMessages(req),
		"metadata": req.Metadata,
	}
	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}

	if req.System != "" {
		sys := map[string]any{"type": "text", "text": req.System}
		if req.CachePrompt {
			sys["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		body["system"] = []map[string]any{sys}
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		}
		body["tools"] = tools
	}
	forcedTool := false
	switch req.ToolChoice {
	case "", "auto":
	case "none":
		delete(body, "tools")
	case "required":
		body["tool_choice"] = map[string]any{"type": "any"}
		forcedTool = true
	default:
		body["tool_choice"] = map[string]any{"type": "tool", "name": req.ToolChoice}
		forcedTool = true
	}

	// Thinking: incompatible with a forced tool choice, so it is skipped there
	// (the schema pass never needs thinking anyway). The output budget must
	// exceed the thinking budget.
	if !forcedTool {
		budget := 0
		switch req.ReasoningEffort {
		case "low":
			budget = 2048
		case "medium", "high":
			budget = 8192
		}
		if budget > 0 {
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			if maxTokens <= budget {
				maxTokens = budget + defaultMaxTokens
			}
		}
	}
	body["max_tokens"] = maxTokens

	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if stream {
		body["stream"] = true
	}
	return json.Marshal(body)
}

// anthropicMessages maps llm messages onto the Anthropic shape: tool-role
// messages become user messages carrying tool_result blocks, and consecutive
// same-role messages are merged (the API requires alternating roles). When
// CachePrompt is set, the last user message's last block gets the second
// cache_control breakpoint (the first is on the system block).
func anthropicMessages(req Request) []map[string]any {
	var msgs []map[string]any
	appendMsg := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
			msgs[n-1]["content"] = append(msgs[n-1]["content"].([]map[string]any), blocks...)
			return
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			var blocks []map[string]any
			for _, p := range m.Parts {
				tr, ok := p.(ToolResultPart)
				if !ok {
					continue
				}
				content := []map[string]any{{"type": "text", "text": tr.Content}}
				for _, f := range tr.Files {
					content = append(content, anthropicMediaBlock(f))
				}
				blocks = append(blocks, map[string]any{
					"type":        "tool_result",
					"tool_use_id": tr.ToolCallID,
					"is_error":    tr.IsError,
					"content":     content,
				})
			}
			appendMsg("user", blocks)
		case RoleAssistant:
			var blocks []map[string]any
			for _, p := range m.Parts {
				switch v := p.(type) {
				case TextPart:
					if v.Text != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": v.Text})
					}
				case ToolCallPart:
					args := strings.TrimSpace(v.Arguments)
					if args == "" {
						args = "{}"
					}
					blocks = append(blocks, map[string]any{
						"type": "tool_use", "id": v.ID, "name": v.Name,
						"input": json.RawMessage(args),
					})
				}
				// ReasoningParts are dropped on replay, mirroring the OpenAI wire:
				// thinking blocks need provider signatures to be re-sent verbatim.
			}
			appendMsg("assistant", blocks)
		default: // user (system is top-level, never in messages)
			var blocks []map[string]any
			for _, p := range m.Parts {
				switch v := p.(type) {
				case TextPart:
					if v.Text != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": v.Text})
					}
				case FilePart:
					blocks = append(blocks, anthropicMediaBlock(v))
				}
			}
			appendMsg("user", blocks)
		}
	}

	if req.CachePrompt {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i]["role"] != "user" {
				continue
			}
			blocks := msgs[i]["content"].([]map[string]any)
			last := blocks[len(blocks)-1]
			// A breakpoint goes on the block itself — for tool_result blocks the
			// API accepts cache_control at the block level.
			last["cache_control"] = map[string]any{"type": "ephemeral"}
			break
		}
	}
	return msgs
}

// anthropicMediaBlock renders a FilePart: images as image blocks, PDFs as
// document blocks.
func anthropicMediaBlock(f FilePart) map[string]any {
	src := map[string]any{
		"type":       "base64",
		"media_type": f.MediaType,
		"data":       base64.StdEncoding.EncodeToString(f.Data),
	}
	if strings.HasPrefix(f.MediaType, "image/") {
		return map[string]any{"type": "image", "source": src}
	}
	return map[string]any{"type": "document", "source": src}
}

// do posts to /messages with retries (shared with the OpenAI wire).
func (c *AnthropicClient) do(ctx context.Context, body []byte) (*http.Response, error) {
	return doWithRetry(ctx, c.HTTP, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("x-api-key", c.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("Content-Type", "application/json")
		if c.UserAgent != "" {
			httpReq.Header.Set("User-Agent", c.UserAgent)
		}
		return httpReq, nil
	})
}

// --- wire types (Anthropic messages) ---

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) toUsage() Usage {
	return Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
}

type anthropicBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

func anthropicFinish(stopReason string) FinishReason {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return FinishStop
	case "tool_use":
		return FinishToolCalls
	case "max_tokens":
		return FinishLength
	default:
		return FinishOther
	}
}

// Complete performs one non-streaming messages call.
func (c *AnthropicClient) Complete(ctx context.Context, req Request) (*Response, error) {
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
		return nil, fmt.Errorf("anthropic %d: %s", httpResp.StatusCode, snippet(data))
	}

	var wire struct {
		Content    []anthropicBlock `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      anthropicUsage   `json:"usage"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode response: %w (%s)", err, snippet(data))
	}

	var parts []Part
	for _, b := range wire.Content {
		switch b.Type {
		case "text":
			parts = append(parts, TextPart{Text: b.Text})
		case "thinking":
			parts = append(parts, ReasoningPart{Text: b.Thinking})
		case "tool_use":
			parts = append(parts, ToolCallPart{ID: b.ID, Name: b.Name, Arguments: string(b.Input)})
		}
	}
	return &Response{
		Content:      parts,
		FinishReason: anthropicFinish(wire.StopReason),
		Usage:        wire.Usage.toUsage(),
	}, nil
}

// StreamComplete performs one streaming messages call over SSE. Input-side
// usage (including cache read/write) arrives in message_start; output tokens
// and the stop reason arrive in message_delta.
func (c *AnthropicClient) StreamComplete(ctx context.Context, req Request) (*Stream, error) {
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
		return nil, fmt.Errorf("anthropic %d: %s", httpResp.StatusCode, snippet(data))
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
		var usage Usage
		stopReason := ""
		aborted := false
		// Open tool_use blocks by content index; arguments accumulate from
		// input_json_delta events.
		type openTool struct{ id, name, args string }
		openTools := map[int]*openTool{}

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
			if payload == "" {
				continue
			}
			var ev struct {
				Type    string `json:"type"`
				Index   int    `json:"index"`
				Message struct {
					Usage anthropicUsage `json:"usage"`
				} `json:"message"`
				ContentBlock anthropicBlock `json:"content_block"`
				Delta        struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
					StopReason  string `json:"stop_reason"`
				} `json:"delta"`
				Usage anthropicUsage `json:"usage"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "message_start":
				u := ev.Message.Usage.toUsage()
				usage.Input = u.Input
				usage.CacheRead = u.CacheRead
				usage.CacheWrite = u.CacheWrite
			case "content_block_start":
				if ev.ContentBlock.Type == "tool_use" {
					openTools[ev.Index] = &openTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				}
			case "content_block_delta":
				switch ev.Delta.Type {
				case "text_delta":
					text += ev.Delta.Text
					select {
					case deltas <- Delta{Text: ev.Delta.Text}:
					case <-ctx.Done():
						aborted = true
					}
				case "thinking_delta":
					reasoning += ev.Delta.Thinking
					select {
					case deltas <- Delta{Reasoning: ev.Delta.Thinking}:
					case <-ctx.Done():
						aborted = true
					}
				case "input_json_delta":
					if t := openTools[ev.Index]; t != nil {
						t.args += ev.Delta.PartialJSON
					}
				}
			case "message_delta":
				if ev.Delta.StopReason != "" {
					stopReason = ev.Delta.StopReason
				}
				if ev.Usage.OutputTokens > 0 {
					usage.Output = ev.Usage.OutputTokens
				}
			case "error":
				finalErr = fmt.Errorf("anthropic stream: %s: %s", ev.Error.Type, ev.Error.Message)
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
		// Emit tool calls in content-index order.
		idxs := make([]int, 0, len(openTools))
		for i := range openTools {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			t := openTools[i]
			args := t.args
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			parts = append(parts, ToolCallPart{ID: t.id, Name: t.name, Arguments: args})
		}

		final = Response{
			Content:      parts,
			FinishReason: anthropicFinish(stopReason),
			Usage:        usage,
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
