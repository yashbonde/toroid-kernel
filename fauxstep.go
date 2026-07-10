package toroid

import (
	"context"
	"sync"

	"charm.land/fantasy"
)

// FauxStep is an in-memory Step for tests: no network, no gateway (M12). It
// replays scripted assistant messages in order and can report fake usage
// (including cache tokens) so loop, cost, and abort behaviour can be exercised
// deterministically. Each call to Complete/Stream/CompleteObject consumes the
// next scripted reply.
type FauxStep struct {
	// Replies are returned in order, one per Complete/Stream call. When exhausted
	// the last reply is repeated (so a stuck loop still terminates on StopReason).
	Replies []FauxReply
	// Object is returned by CompleteObject.
	Object FauxObject
	// OnPayload-style capture: every call appends its PayloadDebug here.
	Payloads []PayloadDebug

	mu   sync.Mutex
	next int
}

// FauxReply scripts one assistant message plus its fake usage and stop reason.
type FauxReply struct {
	Text       string
	ToolCalls  []FauxToolCall // when set, the assistant content includes these tool calls
	Usage      fantasy.Usage  // token counts; Cost is priced from the model like real usage
	StopReason string         // defaults to StopReasonStop when empty
}

// FauxToolCall scripts one tool call in a FauxReply, so the kernel-owned Step
// loop can be tested end-to-end (LLM asks for a tool, kernel runs it).
type FauxToolCall struct {
	ID    string
	Name  string
	Input string
}

// FauxObject scripts the CompleteObject result.
type FauxObject struct {
	Object  any
	RawText string
	Usage   fantasy.Usage
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

func (f *FauxStep) record(p PayloadDebug) {
	f.mu.Lock()
	f.Payloads = append(f.Payloads, p)
	f.mu.Unlock()
}

func (f *FauxStep) assistant(model Model, r FauxReply) *AssistantMessage {
	var u Usage
	u.FromFantasyUsage(r.Usage, model.ID)
	reason := r.StopReason
	if reason == "" {
		if len(r.ToolCalls) > 0 {
			reason = StopReasonToolUse
		} else {
			reason = StopReasonStop
		}
	}
	content := fantasy.ResponseContent{}
	if r.Text != "" {
		content = append(content, fantasy.TextContent{Text: r.Text})
	}
	for _, tc := range r.ToolCalls {
		content = append(content, fantasy.ToolCallContent{
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Input:      tc.Input,
		})
	}
	return &AssistantMessage{
		Content:    content,
		Usage:      u,
		StopReason: reason,
	}
}

// Complete returns the next scripted assistant message.
func (f *FauxStep) Complete(ctx context.Context, model Model, c Context, opts StepOptions) (*AssistantMessage, error) {
	f.record(PayloadDebug{Model: model.ID, Provider: model.Provider, API: model.API, System: c.System, Messages: c.Messages, Tools: toolNames(c.Tools)})
	if opts.OnPayload != nil {
		firePayload(model, c, opts, "", false)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.assistant(model, f.take()), nil
}

// Stream streams the next scripted assistant message one rune-chunk at a time,
// honouring cancellation (partial text + StopReasonAborted) for M11 tests.
func (f *FauxStep) Stream(ctx context.Context, model Model, c Context, opts StepOptions) (*StreamResult, error) {
	f.record(PayloadDebug{Model: model.ID, Provider: model.Provider, API: model.API, System: c.System, Messages: c.Messages, Tools: toolNames(c.Tools), Stream: true})
	if opts.OnPayload != nil {
		firePayload(model, c, opts, "", true)
	}
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
		// Emit the scripted text as whitespace-delimited word chunks so a test can
		// cancel partway and observe partial content.
		for _, word := range splitWords(reply.Text) {
			select {
			case <-ctx.Done():
				aborted = true
			case deltas <- StreamDelta{Text: word}:
				sent += word
			}
			if aborted {
				break
			}
		}
		var u Usage
		u.FromFantasyUsage(reply.Usage, model.ID)
		reason := reply.StopReason
		if reason == "" {
			reason = StopReasonStop
		}
		if aborted {
			reason = StopReasonAborted
			finalErr = ctx.Err()
		}
		final = AssistantMessage{
			Content:    fantasy.ResponseContent{fantasy.TextContent{Text: sent}},
			Usage:      u,
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
func (f *FauxStep) CompleteObject(ctx context.Context, model Model, c Context, sch fantasy.Schema, schemaName, schemaDescription string, opts StepOptions) (*ObjectResult, error) {
	f.record(PayloadDebug{Model: model.ID, Provider: model.Provider, API: model.API, System: c.System, Messages: c.Messages, Tools: toolNames(c.Tools), Schema: schemaName})
	if opts.OnPayload != nil {
		firePayload(model, c, opts, schemaName, false)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var u Usage
	u.FromFantasyUsage(f.Object.Usage, model.ID)
	return &ObjectResult{
		Object:     f.Object.Object,
		RawText:    f.Object.RawText,
		Usage:      u,
		StopReason: StopReasonStop,
	}, nil
}

// splitWords breaks text into space-preserving chunks for streaming emulation.
func splitWords(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			out = append(out, s[start:i+1]) // include the trailing space
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
