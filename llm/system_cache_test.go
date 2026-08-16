package llm

import (
	"encoding/json"
	"testing"
)

func TestAnthropicCachesOnlyInvariantSystemPrefix(t *testing.T) {
	body, err := NewAnthropicClient("key").buildBody(Request{
		Model: "anthropic/test", SystemPrefix: "stable", System: "runtime",
		Messages: []Message{NewUserMessage("hello")}, CachePrompt: true,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.System) != 2 || wire.System[0]["text"] != "stable" || wire.System[1]["text"] != "runtime" {
		t.Fatalf("unexpected system blocks: %#v", wire.System)
	}
	if _, ok := wire.System[0]["cache_control"]; !ok {
		t.Fatal("invariant prefix has no cache breakpoint")
	}
	if _, ok := wire.System[1]["cache_control"]; ok {
		t.Fatal("runtime suffix should not have a cache breakpoint")
	}
}

func TestOpenAICompatibleCachesOnlyInvariantSystemPrefix(t *testing.T) {
	temperature := 0.0
	body, err := NewClient("https://example.com/v1", "key").buildBody(Request{
		Model: "test", SystemPrefix: "stable", System: "runtime",
		Messages: []Message{NewUserMessage("hello")}, CachePrompt: true, Temperature: &temperature,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	blocks := wire.Messages[0].Content
	if len(blocks) != 2 || blocks[0]["text"] != "stable" || blocks[1]["text"] != "runtime" {
		t.Fatalf("unexpected system blocks: %#v", blocks)
	}
	if _, ok := blocks[0]["cache_control"]; !ok {
		t.Fatal("invariant prefix has no cache breakpoint")
	}
	if _, ok := blocks[1]["cache_control"]; ok {
		t.Fatal("runtime suffix should not have a cache breakpoint")
	}
}
