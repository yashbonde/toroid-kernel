package toroid

import (
	oteltrace "go.opentelemetry.io/otel/trace"
)

// OpenTelemetry-native telemetry (§12.2).
//
// The kernel's persisted trace/span graph maps directly onto OpenTelemetry: a
// span is a kernel run, child spans are subagents, and kernel Events become span
// events. OTELSpans converts a stored trace into OTEL-shaped snapshots carrying
// real otel/trace IDs (derived via OTELIDs), so a host can feed them to any OTLP
// exporter without the kernel taking a hard dependency on the OpenTelemetry SDK.

// OTELEvent is a single event recorded against a span.
type OTELEvent struct {
	Name      string `json:"name"`
	TimeUnix  int64  `json:"time_unix_nano"`
	Attribute string `json:"attribute,omitempty"` // JSON payload, if any
}

// OTELSpan is an OpenTelemetry-shaped view of a kernel span.
type OTELSpan struct {
	TraceID      oteltrace.TraceID `json:"trace_id"`
	SpanID       oteltrace.SpanID  `json:"span_id"`
	ParentSpanID oteltrace.SpanID  `json:"parent_span_id"`
	Name         string            `json:"name"`
	StartUnix    int64             `json:"start_unix_nano"`
	EndUnix      int64             `json:"end_unix_nano"`
	Model        string            `json:"model,omitempty"`
	CostUSD      float64           `json:"cost_usd"`
	Events       []OTELEvent       `json:"events,omitempty"`
}

// OTELSpans loads a trace and returns its spans as OpenTelemetry-shaped snapshots
// with spec-valid trace/span IDs and start/end timestamps.
func OTELSpans(traceID string) ([]OTELSpan, error) {
	td, err := LoadTraceData(traceID)
	if err != nil {
		return nil, err
	}
	out := make([]OTELSpan, 0, len(td.Spans))
	for _, sp := range td.Spans {
		tid, sid := OTELIDs(sp.TraceID, sp.SpanID)
		var pid oteltrace.SpanID
		if sp.ParentSpanID != "" {
			_, pid = OTELIDs(sp.TraceID, sp.ParentSpanID)
		}

		name := sp.Title
		if name == "" {
			name = "kernel.run"
		}

		var cost float64
		if n := len(sp.Costs); n > 0 {
			cost = sp.Costs[n-1].TotalUSD
		}

		events := make([]OTELEvent, 0, len(sp.Events))
		for _, ev := range sp.Events {
			events = append(events, OTELEvent{Name: string(ev.Kind), TimeUnix: ev.EmitTS})
		}

		out = append(out, OTELSpan{
			TraceID:      tid,
			SpanID:       sid,
			ParentSpanID: pid,
			Name:         name,
			StartUnix:    sp.StartedAt,
			EndUnix:      sp.EndedAt,
			Model:        sp.Model,
			CostUSD:      cost,
			Events:       events,
		})
	}
	return out, nil
}
