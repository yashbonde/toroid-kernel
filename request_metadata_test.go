package toroid

import (
	"context"
	"strings"
	"testing"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

type metadataRecordingStep struct {
	*FauxStep
	seen []llm.RequestMetadata
}

func (s *metadataRecordingStep) Complete(ctx context.Context, model Model, c Context, opts StepOptions) (*AssistantMessage, error) {
	s.seen = append(s.seen, c.Metadata)
	return s.FauxStep.Complete(ctx, model, c, opts)
}

func (s *metadataRecordingStep) CompleteObject(ctx context.Context, model Model, c Context, schema Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error) {
	s.seen = append(s.seen, c.Metadata)
	return s.FauxStep.CompleteObject(ctx, model, c, schema, schemaName, schemaDescription, opts)
}

func TestKernelAssignsRequestHierarchyMetadata(t *testing.T) {
	ctx := context.Background()
	loadSkills := false
	registry := tools.NewRegistry()
	registry.Register(&tools.ToolDef{
		Name:        "noop",
		Description: "return successfully",
		Handler: llm.NewTool("noop", "return successfully", func(context.Context, struct{}) (llm.ToolResult, error) {
			return llm.NewTextResult("ok"), nil
		}),
	})
	k, err := NewKernel(ctx, Config{
		Model:      "llmgateway/test-model",
		WorkDir:    t.TempDir(),
		LoadSkills: &loadSkills,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()

	step := &metadataRecordingStep{FauxStep: &FauxStep{
		Replies: []FauxReply{
			{ToolCalls: []FauxToolCall{{ID: "call-1", Name: "noop", Input: `{}`}}},
			{Text: "first done"},
			{Text: "second done"},
		},
		Object: FauxObject{Object: map[string]any{"ok": true}},
	}}
	k.Step = step

	if _, _, err := k.Run(ctx, "first chat"); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := k.Stream(ctx, "second chat", &out, WithSchema(
		Schema{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
		"result", "result")); err != nil {
		t.Fatal(err)
	}
	if err := k.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	if len(step.seen) != 5 {
		t.Fatalf("recorded %d requests, want 5", len(step.seen))
	}
	turnIDs := map[string]bool{}
	traceIDs := map[string]bool{}
	for i, metadata := range step.seen {
		if metadata.TranscriptID != k.Cfg.TraceID {
			t.Errorf("request %d transcript_id = %q, want %q", i, metadata.TranscriptID, k.Cfg.TraceID)
		}
		if metadata.ChatID == "" || metadata.TurnID == "" || metadata.TraceID == "" {
			t.Errorf("request %d has incomplete metadata: %+v", i, metadata)
		}
		if turnIDs[metadata.TurnID] {
			t.Errorf("request %d reused turn_id %q", i, metadata.TurnID)
		}
		if traceIDs[metadata.TraceID] {
			t.Errorf("request %d reused trace_id %q", i, metadata.TraceID)
		}
		turnIDs[metadata.TurnID] = true
		traceIDs[metadata.TraceID] = true
	}
	if step.seen[0].ChatID != step.seen[1].ChatID {
		t.Error("tool-loop turns did not share a chat_id")
	}
	if step.seen[2].ChatID != step.seen[3].ChatID {
		t.Error("structured-output request did not share its chat_id")
	}
	if step.seen[0].ChatID == step.seen[2].ChatID || step.seen[2].ChatID == step.seen[4].ChatID {
		t.Error("separate chats reused a chat_id")
	}
}
