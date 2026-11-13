package toroid

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// recordingWriter keeps every Write separately so a test can tell live,
// incremental delivery apart from one write per turn.
type recordingWriter struct {
	mu     sync.Mutex
	writes []string
	err    error
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func (w *recordingWriter) String() string { return strings.Join(w.writes, "") }

// streamTurn scripts one streamed llm-step: reasoning and text deltas, then the
// assembled message (including any tool calls) from Final.
type streamTurn struct {
	reasoning []string
	text      []string
	toolCalls []llm.ToolCallPart
	cost      float64
}

// scriptedStreamStep serves turns only through Stream; Complete fails, so a
// passing test proves the loop took the streaming path.
type scriptedStreamStep struct {
	*FauxStep
	turns []streamTurn
	next  int
}

func (s *scriptedStreamStep) Complete(context.Context, Model, Context, StepOptions) (*AssistantMessage, error) {
	return nil, errors.New("Complete must not be called when token streaming is on")
}

func (s *scriptedStreamStep) Stream(ctx context.Context, _ Model, _ Context, _ StepOptions) (*StreamResult, error) {
	turn := s.turns[s.next]
	s.next++
	deltas := make(chan StreamDelta)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(deltas)
		for _, r := range turn.reasoning {
			deltas <- StreamDelta{Reasoning: r}
		}
		for _, t := range turn.text {
			deltas <- StreamDelta{Text: t}
		}
	}()
	return &StreamResult{
		Deltas: deltas,
		Final: func() (*AssistantMessage, error) {
			<-done
			var content []llm.Part
			if r := strings.Join(turn.reasoning, ""); r != "" {
				content = append(content, llm.ReasoningPart{Text: r})
			}
			if t := strings.Join(turn.text, ""); t != "" {
				content = append(content, llm.TextPart{Text: t})
			}
			reason := StopReasonStop
			for _, tc := range turn.toolCalls {
				content = append(content, tc)
				reason = StopReasonToolUse
			}
			return &AssistantMessage{Content: content, Usage: Usage{Cost: turn.cost, PricingOK: true}, StopReason: reason}, nil
		},
	}, nil
}

func TestTokenStreamWritesTextAsItArrives(t *testing.T) {
	k := newTurnEventTestKernel(t, tools.NewRegistry())
	k.Step = &FauxStep{Replies: []FauxReply{{Text: "alpha beta gamma delta", Cost: 0.01}}}

	var out recordingWriter
	if err := k.Stream(context.Background(), "hi", &out, WithTokenStream()); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "alpha beta gamma delta" {
		t.Fatalf("text = %q", got)
	}
	if len(out.writes) < 4 {
		t.Fatalf("got %d writes %q; want one per streamed chunk", len(out.writes), out.writes)
	}
	if got := k.RunningCostUSD(); got != 0.01 {
		t.Fatalf("RunningCostUSD = %v; a streamed step must still be billed", got)
	}
}

func TestDefaultRunWritesEachTurnOnce(t *testing.T) {
	k := newTurnEventTestKernel(t, tools.NewRegistry())
	k.Step = &FauxStep{Replies: []FauxReply{{Text: "alpha beta gamma"}}}

	var out recordingWriter
	if err := k.Stream(context.Background(), "hi", &out); err != nil {
		t.Fatal(err)
	}
	if len(out.writes) != 1 || out.writes[0] != "alpha beta gamma" {
		t.Fatalf("writes = %q; want the whole turn in one write", out.writes)
	}
}

func TestReasoningStreamAcrossToolTurn(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&tools.ToolDef{
		Name: "ok",
		Handler: llm.NewTool("ok", "succeed", func(context.Context, struct{}) (llm.ToolResult, error) {
			return llm.NewTextResult("ok"), nil
		}),
	})
	k := newTurnEventTestKernel(t, registry)
	k.Step = &scriptedStreamStep{FauxStep: &FauxStep{}, turns: []streamTurn{
		{reasoning: []string{"need ", "a tool"}, text: []string{"Checking", ". "}, toolCalls: []llm.ToolCallPart{{ID: "c1", Name: "ok", Arguments: `{}`}}, cost: 0.002},
		{reasoning: []string{"done now"}, text: []string{"All ", "good."}, cost: 0.003},
	}}
	var reasoningEvents []string
	k.On(EventReasoning, func(_ context.Context, e Event) error {
		reasoningEvents = append(reasoningEvents, e.Payload.(*ReasoningPayload).Text)
		return nil
	})

	var out, thinking recordingWriter
	if err := k.Stream(context.Background(), "go", &out, WithReasoningStream(&thinking)); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Checking. All good." {
		t.Fatalf("text = %q; streamed text must not be written twice", got)
	}
	if got := thinking.String(); got != "need a tooldone now" {
		t.Fatalf("reasoning stream = %q", got)
	}
	if len(reasoningEvents) != 2 || reasoningEvents[0] != "need a tool" || reasoningEvents[1] != "done now" {
		t.Fatalf("EventReasoning = %q; want one full block per turn", reasoningEvents)
	}
	if got := k.RunningCostUSD(); got < 0.0049 || got > 0.0051 {
		t.Fatalf("RunningCostUSD = %v; want both turns billed", got)
	}
}

func TestTokenStreamWriteErrorDoesNotHang(t *testing.T) {
	k := newTurnEventTestKernel(t, tools.NewRegistry())
	k.Step = &FauxStep{Replies: []FauxReply{{Text: "one two three four five six"}}}

	out := &recordingWriter{err: errors.New("client went away")}
	done := make(chan error, 1)
	go func() { done <- k.Stream(context.Background(), "hi", out, WithTokenStream()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "client went away") {
			t.Fatalf("err = %v; want the writer's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stream hung after the writer failed")
	}
}
