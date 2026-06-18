package toroid

import (
	"strings"
	"testing"
)

// TestOTELSpansCanonical verifies the unified OTEL projection: payloads survive
// into span-event attributes (previously dropped), GenAI semantic-convention
// attributes are derived from stored usage/cost, and non-observable event kinds
// are filtered out.
func TestOTELSpansCanonical(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dbMu.Lock()
	defaultDB = nil
	dbMu.Unlock()

	st, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer st.Close()

	trace := NewSessionID()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.SaveTraceMeta(TraceMeta{TraceID: trace, Title: "t", StartedAt: 1}))
	must(st.SaveSpanMeta(SpanMeta{SpanID: trace, TraceID: trace, Model: "anthropic/claude-haiku-4-5", StartedAt: 1, EndedAt: 9}))
	must(st.AppendCost(trace, trace, 0.002, 0.002))

	// A tool call (payload must survive) ...
	must(st.AppendEvent(trace, trace, Event{
		Kind: EventPreToolUse, TraceID: trace, SpanID: trace, EmitTS: 2, Seq: 1,
		Payload: ToolUsePayload{Name: "read_file", Args: `{"path":"go.mod"}`},
	}))
	// ... a turn cost carrying token usage ...
	must(st.AppendEvent(trace, trace, Event{
		Kind: EventTurnCost, TraceID: trace, SpanID: trace, EmitTS: 3, Seq: 2,
		Payload: TurnCostPayload{TurnUsage: Usage{Input: 100, Output: 40, Reasoning: 5}, TurnCostUSD: 0.002, TotalCostUSD: 0.002},
	}))
	// ... and a non-observable control-plane event that must be filtered out.
	must(st.AppendEvent(trace, trace, Event{
		Kind: EventMasterIdle, TraceID: trace, SpanID: trace, EmitTS: 4, Seq: 3,
	}))

	spans, err := OTELSpans(trace)
	if err != nil {
		t.Fatalf("OTELSpans: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]

	// MasterIdle filtered; the two observable events remain.
	if len(s.Events) != 2 {
		t.Fatalf("got %d events, want 2 (MasterIdle should be filtered): %+v", len(s.Events), s.Events)
	}
	// Payload survived into the span-event attribute (the previously-dropped bug).
	var sawTool bool
	for _, e := range s.Events {
		if e.Name == string(EventPreToolUse) {
			sawTool = true
			if !strings.Contains(e.Attribute, "read_file") || !strings.Contains(e.Attribute, "go.mod") {
				t.Fatalf("tool payload not preserved in attribute: %q", e.Attribute)
			}
		}
	}
	if !sawTool {
		t.Fatal("PreToolUse event missing from export")
	}

	// GenAI semantic-convention attributes derived from usage + cost.
	attrs := map[string]any{}
	for _, a := range s.Attributes {
		attrs[a.Key] = a.Value
	}
	if attrs["gen_ai.request.model"] != "anthropic/claude-haiku-4-5" {
		t.Fatalf("model attr = %v", attrs["gen_ai.request.model"])
	}
	if attrs["gen_ai.usage.input_tokens"] != int64(100) {
		t.Fatalf("input_tokens attr = %v (want 100)", attrs["gen_ai.usage.input_tokens"])
	}
	if attrs["gen_ai.usage.output_tokens"] != int64(40) {
		t.Fatalf("output_tokens attr = %v (want 40)", attrs["gen_ai.usage.output_tokens"])
	}
	if attrs["gen_ai.usage.total_tokens"] != int64(140) {
		t.Fatalf("total_tokens attr = %v (want 140)", attrs["gen_ai.usage.total_tokens"])
	}
	if c, ok := attrs["gen_ai.usage.cost_usd"].(float64); !ok || c != 0.002 {
		t.Fatalf("cost_usd attr = %v (want 0.002)", attrs["gen_ai.usage.cost_usd"])
	}
}
