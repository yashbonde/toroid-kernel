package llm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

// splitChoicesBody is the non-streaming response shape the gateway returns for
// a Fireworks-hosted DeepSeek model: the assistant text and the tool call
// arrive as two separate choices, and neither carries a finish_reason.
const splitChoicesBody = `{
  "choices": [
    {"index": 0, "message": {"role": "assistant", "content": "I'll list the files."}},
    {"index": 1, "message": {"role": "assistant", "tool_calls": [
      {"index": 0, "type": "function", "id": "call_1",
       "function": {"name": "bash", "arguments": "{\"command\":\"ls\"}"}}
    ]}}
  ],
  "usage": {"prompt_tokens": 10, "completion_tokens": 5}
}`

func completeWithBody(t *testing.T, body string) *Response {
	t.Helper()
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Request:    req,
		}, nil
	})}
	c := &Client{BaseURL: "http://llm.test/v1", HTTP: httpClient}
	resp, err := c.Complete(context.Background(), Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return resp
}

// Reading only choices[0] dropped the tool call, so the kernel loop saw a
// text-only turn and ended the run after "I'll list the files."
func TestCompleteKeepsToolCallsFromLaterChoices(t *testing.T) {
	resp := completeWithBody(t, splitChoicesBody)

	if got := resp.Text(); got != "I'll list the files." {
		t.Fatalf("Text = %q, want the choice-0 text", got)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1 (from choice 1)", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "bash" || calls[0].Arguments != `{"command":"ls"}` {
		t.Fatalf("ToolCall = %+v, want call_1 bash {\"command\":\"ls\"}", calls[0])
	}
}

func TestCompleteInfersToolCallsFinishWhenMissing(t *testing.T) {
	resp := completeWithBody(t, splitChoicesBody)

	if resp.FinishReason != FinishToolCalls {
		t.Fatalf("FinishReason = %q, want %q when tool calls arrive without finish_reason", resp.FinishReason, FinishToolCalls)
	}
}
