// Pattern: PERSISTENCE & OPENTELEMETRY (no LLM key required).
//
// With Config.Save = true a kernel persists its traces, spans, costs, and events
// to one SQLite database (~/.swarmbuddy/sql.db). The trace/span/parent graph maps
// directly onto OpenTelemetry: toroid.OTELSpans(traceID) returns OTEL-shaped spans
// (spec-valid 16-byte trace / 8-byte span IDs, derived from time-ordered Snowflake
// IDs) ready to hand to any OTLP exporter.
//
// This example writes a synthetic trace straight to the Store (so it runs without
// any API key or network), exports it as OTEL spans, lists sessions, then deletes
// the synthetic trace to clean up after itself.
//
//	go run ./examples/otel
package main

import (
	"fmt"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	store, err := toroid.NewStore()
	if err != nil {
		panic(err)
	}
	defer store.Close()

	// A trace with a root span and one child span (e.g. a subagent).
	traceID := toroid.NewSessionID()
	childID := toroid.NewSessionID()

	must(store.SaveTraceMeta(toroid.TraceMeta{TraceID: traceID, Title: "otel example", StartedAt: 1}))
	must(store.SaveSpanMeta(toroid.SpanMeta{SpanID: traceID, TraceID: traceID, Model: "google/gemini-3-flash-preview", StartedAt: 1, EndedAt: 9}))
	must(store.SaveSpanMeta(toroid.SpanMeta{SpanID: childID, TraceID: traceID, ParentSpanID: traceID, Title: "subagent", StartedAt: 2, EndedAt: 5}))
	must(store.AppendCost(traceID, traceID, 0.001, 0.001))
	must(store.AppendEvent(traceID, traceID, toroid.Event{Kind: toroid.EventUserPromptSubmit, TraceID: traceID, SpanID: traceID, EmitTS: 1, Seq: 1}))

	// Export as OpenTelemetry-shaped spans.
	spans, err := toroid.OTELSpans(traceID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("OTEL spans for trace %s:\n", traceID)
	for _, s := range spans {
		fmt.Printf("  span=%s parent=%s name=%-9s trace=%s cost=$%.4f\n",
			s.SpanID, s.ParentSpanID, s.Name, s.TraceID, s.CostUSD)
	}

	// List sessions (newest first).
	sessions, err := toroid.ListSessions()
	if err != nil {
		panic(err)
	}
	fmt.Printf("\n%d session(s) in the store\n", len(sessions))

	// Clean up the synthetic trace.
	must(toroid.DeleteSession(traceID))
	fmt.Println("cleaned up synthetic trace")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
