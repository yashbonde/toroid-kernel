package toroid

import (
	"context"
	"errors"
	"testing"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

type failingCompleteStep struct {
	*FauxStep
	err error
}

func (s *failingCompleteStep) Complete(context.Context, Model, Context, StepOptions) (*AssistantMessage, error) {
	return nil, s.err
}

func newTurnEventTestKernel(t *testing.T, registry *tools.Registry) *Kernel {
	t.Helper()
	loadSkills := false
	k, err := NewKernel(context.Background(), Config{
		Model:      "llmgateway/test-model",
		WorkDir:    t.TempDir(),
		LoadSkills: &loadSkills,
		Tools:      registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k
}

func TestTurnLifecycleEventsCompleteExactlyOnce(t *testing.T) {
	ctx := context.Background()
	registry := tools.NewRegistry()
	registry.Register(&tools.ToolDef{
		Name: "ok",
		Handler: llm.NewTool("ok", "succeed", func(context.Context, struct{}) (llm.ToolResult, error) {
			return llm.NewTextResult("ok"), nil
		}),
	})
	registry.Register(&tools.ToolDef{
		Name: "fail",
		Handler: llm.NewTool("fail", "return a tool error", func(context.Context, struct{}) (llm.ToolResult, error) {
			return llm.ToolResult{}, errors.New("tool failed")
		}),
	})

	k := newTurnEventTestKernel(t, registry)
	k.Step = &FauxStep{Replies: []FauxReply{
		{ToolCalls: []FauxToolCall{
			{ID: "call-1", Name: "ok", Input: `{}`},
			{ID: "call-2", Name: "fail", Input: `{}`},
		}},
		{Text: "done"},
	}}

	// v2: scope ids (TranscriptID/ChatID/TurnID/LLMStepID) live on the Event
	// envelope, not the TurnPayload, so capture full events to check them.
	var started, completed []Event
	var failed []TurnPayload
	k.OnAll(func(_ context.Context, event Event) error {
		switch event.Kind {
		case EventTurnStarted:
			started = append(started, event)
		case EventTurnCompleted:
			completed = append(completed, event)
		case EventTurnFailed:
			failed = append(failed, *event.Payload.(*TurnPayload))
		}
		return nil
	})

	out, _, err := k.Run(ctx, "run both tools")
	if err != nil {
		t.Fatal(err)
	}
	if out != "done" {
		t.Fatalf("output = %q, want done", out)
	}
	if len(started) != 2 || len(completed) != 2 || len(failed) != 0 {
		t.Fatalf("turn events: started=%d completed=%d failed=%d, want 2/2/0", len(started), len(completed), len(failed))
	}

	for i := range started {
		start, end := started[i], completed[i]
		if start.TranscriptID == "" || start.ChatID == "" || start.TurnID == "" || start.LLMStepID == "" {
			t.Fatalf("turn %d has incomplete hierarchy: %+v", i, start)
		}
		if start.TranscriptID != end.TranscriptID || start.ChatID != end.ChatID || start.TurnID != end.TurnID || start.LLMStepID != end.LLMStepID {
			t.Errorf("turn %d terminal event changed hierarchy: start=%+v end=%+v", i, start, end)
		}
	}
	completedPayload := func(e Event) TurnPayload { return *e.Payload.(*TurnPayload) }
	if p := completedPayload(completed[0]); p.StopReason != StopReasonToolUse || p.ToolCalls != 2 {
		t.Errorf("tool turn completion = %+v", p)
	}
	if p := completedPayload(completed[1]); p.StopReason != StopReasonStop || p.ToolCalls != 0 {
		t.Errorf("final turn completion = %+v", p)
	}
}

func TestTurnLifecycleEventFailsExactlyOnce(t *testing.T) {
	wantErr := errors.New("gateway unavailable")
	k := newTurnEventTestKernel(t, tools.NewRegistry())
	k.Step = &failingCompleteStep{FauxStep: &FauxStep{}, err: wantErr}

	var started, failed []Event
	var completed []Event
	for _, kind := range []EventKind{EventTurnStarted, EventTurnCompleted, EventTurnFailed} {
		kind := kind
		k.On(kind, func(_ context.Context, event Event) error {
			switch kind {
			case EventTurnStarted:
				started = append(started, event)
			case EventTurnCompleted:
				completed = append(completed, event)
			case EventTurnFailed:
				failed = append(failed, event)
			}
			return nil
		})
	}

	if _, _, err := k.Run(context.Background(), "fail"); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want %v", err, wantErr)
	}
	if len(started) != 1 || len(completed) != 0 || len(failed) != 1 {
		t.Fatalf("turn events: started=%d completed=%d failed=%d, want 1/0/1", len(started), len(completed), len(failed))
	}
	// v2: scope ids live on the Event envelope, not the TurnPayload.
	if failed[0].TurnID != started[0].TurnID || failed[0].LLMStepID != started[0].LLMStepID {
		t.Errorf("failed event does not terminate started turn: start=%+v failed=%+v", started[0], failed[0])
	}
	failedPayload := *failed[0].Payload.(*TurnPayload)
	if failedPayload.StopReason != StopReasonError || failedPayload.Error != wantErr.Error() {
		t.Errorf("failure payload = %+v", failedPayload)
	}
}
