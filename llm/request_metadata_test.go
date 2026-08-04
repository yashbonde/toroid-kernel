package llm

import (
	"encoding/json"
	"testing"
)

func TestRequestBodiesIncludeHierarchyMetadata(t *testing.T) {
	want := RequestMetadata{
		TranscriptID: "transcript-1",
		ChatID:       "chat-2",
		TurnID:       "turn-3",
		TraceID:      "trace-4",
	}
	req := Request{Model: "test-model", Metadata: want}

	tests := []struct {
		name  string
		build func() ([]byte, error)
	}{
		{
			name: "openai-compatible",
			build: func() ([]byte, error) {
				return NewClient("https://example.com/v1", "key").buildBody(req, false)
			},
		},
		{
			name: "anthropic",
			build: func() ([]byte, error) {
				return NewAnthropicClient("key").buildBody(req, false)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := tt.build()
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Metadata RequestMetadata `json:"metadata"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Metadata != want {
				t.Fatalf("metadata = %+v, want %+v", envelope.Metadata, want)
			}
		})
	}
}
