package toroid

import (
	"context"
	"strings"
	"sync"

	"github.com/yashbonde/toroid-kernel/llm"
)

// FauxStep is an in-memory Step for tests: no network, no gateway. It replays
// scripted assistant messages in order — each Complete/Stream/CompleteObject
// call consumes the next scripted reply — so loop, cost, and abort behaviour
// can be exercised deterministically.
type FauxStep struct {
	// Replies are returned in order, one per Complete/Stream call. When exhausted
	// the last reply is repeated (so a stuck loop still terminates on StopReason).
	Replies []FauxReply
	// Object is returned by CompleteObject.
	Object FauxObject

	mu   sync.Mutex
	next int
}

// FauxReply scripts one assistant message plus its fake usage and stop reason.
// Cost is the per-call cost.
type FauxReply struct {
	Text       string
	ToolCalls  []FauxToolCall // when set, the assistant content includes these tool calls
	Usage      llm.Usage
	Cost       float64
	StopReason string // defaults by content: toolUse when ToolCalls set, else stop
}

// FauxToolCall scripts one tool call in a FauxReply.
type FauxToolCall struct {
	ID    string
	Name  string
	Input string
}

// FauxObject scripts the CompleteObject result.
type FauxObject struct {
	Object  any
	RawText string
	Usage   llm.Usage
	Cost    float64
}

var _ Step = (*FauxStep)(nil)

func (f *FauxStep) take() FauxReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Replies) == 0 {
		return FauxReply{StopReason: StopReasonStop}
	}
	i := f.next
	if i >= len(f.Replies) {
		i = len(f.Replies) - 1 // repeat the last scripted reply
	} else {
		f.next++
	}
	return f.Replies[i]
}

func fauxUsage(wire llm.Usage, cost float64) Usage {
	return Usage{
		Input:      wire.Input,
		Output:     wire.Output,
		Reasoning:  wire.Reasoning,
		CacheRead:  wire.CacheRead,
		CacheWrite: wire.CacheWrite,
		Cost:       cost,
		PricingOK:  cost > 0,
	}
}

// Complete returns the next scripted assistant message.
func (f *FauxStep) Complete(ctx context.Context, model Model, c Context, opts StepOptions) (*AssistantMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := f.take()
	reason := r.StopReason
	if reason == "" {
		if len(r.ToolCalls) > 0 {
			reason = StopReasonToolUse
		} else {
			reason = StopReasonStop
		}
	}
	var content []llm.Part
	if r.Text != "" {
		content = append(content, llm.TextPart{Text: r.Text})
	}
	for _, tc := range r.ToolCalls {
		content = append(content, llm.ToolCallPart{ID: tc.ID, Name: tc.Name, Arguments: tc.Input})
	}
	return &AssistantMessage{Content: content, Usage: fauxUsage(r.Usage, r.Cost), StopReason: reason}, nil
}

// Stream streams the next scripted assistant message one word-chunk at a time,
// honouring cancellation (partial text + StopReasonAborted).
func (f *FauxStep) Stream(ctx context.Context, model Model, c Context, opts StepOptions) (*StreamResult, error) {
	reply := f.take()

	deltas := make(chan StreamDelta)
	done := make(chan struct{})
	var final AssistantMessage
	var finalErr error

	go func() {
		defer close(done)
		defer close(deltas)
		var sent string
		aborted := false
		// Emit the scripted text word by word so a test can cancel partway and
		// observe partial content.
		words := reply.Text
		for len(words) > 0 && !aborted {
			chunk := words
			if i := strings.IndexByte(words, ' '); i >= 0 {
				chunk = words[:i+1]
			}
			words = words[len(chunk):]
			select {
			case <-ctx.Done():
				aborted = true
			case deltas <- StreamDelta{Text: chunk}:
				sent += chunk
			}
		}
		reason := reply.StopReason
		if reason == "" {
			reason = StopReasonStop
		}
		if aborted {
			reason = StopReasonAborted
			finalErr = ctx.Err()
		}
		final = AssistantMessage{
			Content:    []llm.Part{llm.TextPart{Text: sent}},
			Usage:      fauxUsage(reply.Usage, reply.Cost),
			StopReason: reason,
		}
	}()

	return &StreamResult{
		Deltas: deltas,
		Final: func() (*AssistantMessage, error) {
			<-done
			return &final, finalErr
		},
	}, nil
}

// CompleteObject returns the scripted object result.
func (f *FauxStep) CompleteObject(ctx context.Context, model Model, c Context, sch Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &ObjectResult{
		Object:     f.Object.Object,
		RawText:    f.Object.RawText,
		Usage:      fauxUsage(f.Object.Usage, f.Object.Cost),
		StopReason: StopReasonStop,
	}, nil
}
