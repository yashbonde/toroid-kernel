package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenRouter names the reasoning field "reasoning" (not "reasoning_content")
// and reports billed cost in usage.cost rather than a header. Fixtures mirror
// a captured OpenRouter stream.
func TestOpenRouterStreamReasoningAndBodyCost(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"role":"assistant","reasoning":"Seventeen ","reasoning_details":[{"type":"reasoning.text","text":"Seventeen "}]}}]}`,
		`{"choices":[{"delta":{"reasoning":"times three."}}]}`,
		`{"choices":[{"delta":{"content":"Fifty"}}]}`,
		`{"choices":[{"delta":{"content":"-one"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":21,"completion_tokens":40,"cost":2.5e-05,"is_byok":false,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":3},"completion_tokens_details":{"reasoning_tokens":30}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	stream, err := NewClient(srv.URL, "k").StreamComplete(context.Background(), Request{Model: "qwen/qwen3.7-flash", Messages: []Message{NewUserMessage("17*3?")}})
	if err != nil {
		t.Fatal(err)
	}
	var text, reasoning strings.Builder
	for d := range stream.Deltas {
		text.WriteString(d.Text)
		reasoning.WriteString(d.Reasoning)
	}
	resp, err := stream.Final()
	if err != nil {
		t.Fatal(err)
	}
	if got := reasoning.String(); got != "Seventeen times three." {
		t.Errorf("streamed reasoning = %q", got)
	}
	if got := text.String(); got != "Fifty-one" {
		t.Errorf("streamed text = %q", got)
	}
	var final string
	for _, p := range resp.Content {
		if rp, ok := p.(ReasoningPart); ok {
			final = rp.Text
		}
	}
	if final != "Seventeen times three." {
		t.Errorf("final reasoning part = %q", final)
	}
	if resp.Gateway.CostUSD == nil || *resp.Gateway.CostUSD != 2.5e-05 {
		t.Errorf("CostUSD = %v, want 2.5e-05 from usage.cost", resp.Gateway.CostUSD)
	}
	if resp.Usage.CacheWrite != 3 || resp.Usage.Reasoning != 30 {
		t.Errorf("usage = %+v, want CacheWrite 3 (cache_write_tokens) and Reasoning 30", resp.Usage)
	}
}

func TestOpenRouterCompleteReasoningAndCost(t *testing.T) {
	cases := []struct {
		name     string
		usage    string
		header   string
		wantCost float64
	}{
		{"body cost", `{"prompt_tokens":5,"completion_tokens":2,"cost":0.0001}`, "", 0.0001},
		{"byok adds upstream", `{"prompt_tokens":5,"completion_tokens":2,"cost":0.00002,"is_byok":true,"cost_details":{"upstream_inference_cost":0.0003}}`, "", 0.00032},
		{"header wins over body", `{"prompt_tokens":5,"completion_tokens":2,"cost":0.0001}`, "0.5", 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("x-litellm-response-cost", tc.header)
				}
				fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok","reasoning":"think"}}],"usage":%s}`, tc.usage)
			}))
			defer srv.Close()

			resp, err := NewClient(srv.URL, "k").Complete(context.Background(), Request{Model: "m", Messages: []Message{NewUserMessage("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Gateway.CostUSD == nil || fmt.Sprintf("%.8f", *resp.Gateway.CostUSD) != fmt.Sprintf("%.8f", tc.wantCost) {
				t.Errorf("CostUSD = %v, want %v", resp.Gateway.CostUSD, tc.wantCost)
			}
			if len(resp.Content) == 0 {
				t.Fatal("no content")
			}
			if rp, ok := resp.Content[0].(ReasoningPart); !ok || rp.Text != "think" {
				t.Errorf("first part = %#v, want ReasoningPart{think}", resp.Content[0])
			}
		})
	}
}

// A gateway that reports no cost anywhere must leave CostUSD nil (unknown),
// never a fake zero.
func TestNoCostReportedStaysUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok","reasoning_content":"rc"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	resp, err := NewClient(srv.URL, "k").Complete(context.Background(), Request{Model: "m", Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Gateway.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil", *resp.Gateway.CostUSD)
	}
	if rp, ok := resp.Content[0].(ReasoningPart); !ok || rp.Text != "rc" {
		t.Errorf("reasoning_content not decoded: %#v", resp.Content[0])
	}
}
