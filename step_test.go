package toroid

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// TestFauxStepCompleteScriptedAndPriced verifies the network-free Step (M12):
// scripted replies are returned in order and usage is priced from the catalog.
func TestFauxStepCompleteScriptedAndPriced(t *testing.T) {
	model := ResolveModel("llmgateway/claude-sonnet-4-5")
	faux := &FauxStep{Replies: []FauxReply{
		{Text: "first", Usage: fantasy.Usage{InputTokens: 1000, OutputTokens: 100}, StopReason: StopReasonToolUse},
		{Text: "second", Usage: fantasy.Usage{InputTokens: 2000, OutputTokens: 200}, StopReason: StopReasonStop},
	}}

	m1, err := faux.Complete(context.Background(), model, Context{}, StepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m1.Text() != "first" || m1.StopReason != StopReasonToolUse {
		t.Errorf("step 1 = %q/%q", m1.Text(), m1.StopReason)
	}
	if !m1.Usage.PricingOK || m1.Usage.Cost <= 0 {
		t.Errorf("step 1 usage not priced: %+v", m1.Usage)
	}

	m2, _ := faux.Complete(context.Background(), model, Context{}, StepOptions{})
	if m2.Text() != "second" || m2.StopReason != StopReasonStop {
		t.Errorf("step 2 = %q/%q", m2.Text(), m2.StopReason)
	}
}

// TestFauxStepUnpricedModelHonest verifies Step usage carries PricingOK=false
// for a model absent from the table (M2 honesty flows through the Step layer).
func TestFauxStepUnpricedModelHonest(t *testing.T) {
	model := ResolveModel("llmgateway/unknown-model-xyz")
	faux := &FauxStep{Replies: []FauxReply{{Text: "hi", Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5}}}}
	m, _ := faux.Complete(context.Background(), model, Context{}, StepOptions{})
	if m.Usage.PricingOK {
		t.Error("expected PricingOK=false for unpriced model")
	}
	if m.Usage.Cost != 0 {
		t.Errorf("expected Cost 0, got %v", m.Usage.Cost)
	}
}

// TestKernelOwnedLoopOverStep demonstrates the target architecture (M4): a
// caller can loop Step per turn, inspecting/budgeting between llm-steps, with the
// tool loop owned outside the Step. Cost rolls up across steps.
func TestKernelOwnedLoopOverStep(t *testing.T) {
	model := ResolveModel("llmgateway/claude-sonnet-4-5")
	faux := &FauxStep{Replies: []FauxReply{
		{Text: "call a tool", Usage: fantasy.Usage{InputTokens: 1000, OutputTokens: 50}, StopReason: StopReasonToolUse},
		{Text: "call another", Usage: fantasy.Usage{InputTokens: 1200, OutputTokens: 60}, StopReason: StopReasonToolUse},
		{Text: "done", Usage: fantasy.Usage{InputTokens: 1400, OutputTokens: 70}, StopReason: StopReasonStop},
	}}

	var steps int
	var total float64
	msgs := []fantasy.Message{fantasy.NewUserMessage("do work")}
	for {
		m, err := faux.Complete(context.Background(), model, Context{System: "sys", Messages: msgs}, StepOptions{})
		if err != nil {
			t.Fatal(err)
		}
		steps++
		total += m.Usage.Cost
		if m.StopReason != StopReasonToolUse {
			break // end of chat — tools would run here between llm-steps
		}
		// simulate a tool turn appended between llm-steps
		msgs = append(msgs, fantasy.NewUserMessage("tool result"))
	}
	if steps != 3 {
		t.Errorf("expected 3 llm-steps, got %d", steps)
	}
	if total <= 0 {
		t.Errorf("expected positive rolled-up cost, got %v", total)
	}
}

// TestFauxStepStreamAbortKeepsPartial verifies M11: cancelling mid-step keeps the
// partial assistant content and marks the stop reason aborted.
func TestFauxStepStreamAbortKeepsPartial(t *testing.T) {
	model := ResolveModel("llmgateway/claude-sonnet-4-5")
	faux := &FauxStep{Replies: []FauxReply{{Text: "one two three four five", StopReason: StopReasonStop}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sr, err := faux.Stream(ctx, model, Context{}, StepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var got string
	var n int
	for d := range sr.Deltas {
		got += d.Text
		n++
		if n == 2 {
			cancel() // abort after two chunks
		}
	}
	final, ferr := sr.Final()
	if ferr == nil {
		t.Error("expected a non-nil error (context cancelled) after abort")
	}
	if final.StopReason != StopReasonAborted {
		t.Errorf("StopReason = %q, want aborted", final.StopReason)
	}
	if final.Text() == "" || final.Text() != got {
		t.Errorf("partial content mismatch: final=%q streamed=%q", final.Text(), got)
	}
	if strings.Contains(final.Text(), "five") {
		t.Errorf("aborted stream should not contain the full text: %q", final.Text())
	}
}

// TestStepOnPayloadDebug verifies the M10 debug hook receives the outbound shape:
// single system, tool names, and schema name for object calls.
func TestStepOnPayloadDebug(t *testing.T) {
	model := ResolveModel("llmgateway/claude-sonnet-4-5")
	faux := &FauxStep{Object: FauxObject{Object: map[string]any{"ok": true}}}

	var seen PayloadDebug
	opts := StepOptions{OnPayload: func(p PayloadDebug) { seen = p }}

	sch := fantasy.Schema{}
	_, err := faux.CompleteObject(context.Background(), model, Context{
		System:   "you are a bot",
		Messages: []fantasy.Message{fantasy.NewUserMessage("hi")},
	}, sch, "Findings", "the findings", opts)
	if err != nil {
		t.Fatal(err)
	}
	if seen.System != "you are a bot" {
		t.Errorf("payload system = %q", seen.System)
	}
	if seen.Schema != "Findings" {
		t.Errorf("payload schema = %q, want Findings", seen.Schema)
	}
	if seen.Model != model.ID {
		t.Errorf("payload model = %q", seen.Model)
	}
}
