package toroid

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yashbonde/toroid-kernel/llm"
)

// OTLP/HTTP JSON wire types — the minimal subset of the OpenTelemetry trace
// protocol needed to ship spans to any OTLP backend (Langfuse, SigNoz, …).
// Per the OTLP/JSON spec, trace/span IDs are hex strings and fixed64 timestamps
// are decimal strings; int attribute values are strings, doubles are numbers.
type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpKeyValue struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

// attr builds a string-valued OTLP attribute. Langfuse reads its input/output,
// model, usage, and session/user fields from string attributes, so the rich
// projection uses strings throughout (JSON-encoded where a value is structured).
func attr(key, val string) otlpKeyValue {
	return otlpKeyValue{Key: key, Value: map[string]any{"stringValue": val}}
}

// jsonAttr JSON-encodes v into a string attribute (for input/output/usage/cost).
func jsonAttr(key string, v any) otlpKeyValue {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprintf("%v", v))
	}
	return attr(key, string(b))
}

// childSpanID derives a deterministic, non-zero 8-byte span ID for the nth
// synthetic child of a parent span, so re-exporting a trace is idempotent.
func childSpanID(parentHex string, n int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(parentHex))
	_ = binary.Write(h, binary.BigEndian, int64(n))
	v := h.Sum64()
	if v == 0 {
		v = 1
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return hex.EncodeToString(b[:])
}

// payloadOf re-marshals a loaded event payload (a decoded map) into a typed T.
func payloadOf[T any](ev Event) (T, bool) {
	var p T
	if ev.Payload == nil {
		return p, false
	}
	b, err := json.Marshal(ev.Payload)
	if err != nil {
		return p, false
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, false
	}
	return p, true
}

// turnView is the assistant content extracted from one AssistantTurn event.
type turnView struct {
	Text      string           `json:"content,omitempty"`
	ToolCalls []map[string]any `json:"tool_calls,omitempty"`
	Reasoning string           `json:"reasoning,omitempty"`
}

// summarizeTurn pulls assistant text, reasoning, and tool calls out of the
// serialized []llm.Message captured for an assistant turn.
func summarizeTurn(raw json.RawMessage) turnView {
	var tv turnView
	var msgs []llm.Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return tv
	}
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, part := range m.Parts {
			switch p := part.(type) {
			case llm.TextPart:
				tv.Text += p.Text
			case llm.ReasoningPart:
				tv.Reasoning += p.Text
			case llm.ToolCallPart:
				tv.ToolCalls = append(tv.ToolCalls, map[string]any{"name": p.Name, "input": p.Arguments})
			}
		}
	}
	return tv
}

// usageDetails renders Langfuse usage_details from a Usage.
func usageDetails(u Usage) map[string]any {
	d := map[string]any{}
	put := func(k string, n int64) {
		if n > 0 {
			d[k] = n
		}
	}
	put("input", u.Input)
	put("output", u.Output)
	put("reasoning", u.Reasoning)
	put("cache_read", u.CacheRead)
	put("cache_write", u.CacheWrite)
	if t := u.Input + u.Output; t > 0 {
		d["total"] = t
	}
	return d
}

// buildLangfuseRequest projects a full stored trace into OTLP spans enriched with
// Langfuse semantic attributes: trace/observation input & output, model, token
// usage and cost, session id, and one child observation per assistant turn and
// per tool call — so the Langfuse UI shows everything the kernel emits.
func buildLangfuseRequest(td TraceData) otlpRequest {
	var spans []otlpSpan

	for _, sp := range td.Spans {
		tid, sid := OTELIDs(sp.TraceID, sp.SpanID)
		traceHex, rootHex := tid.String(), sid.String()

		name := sp.Title
		if name == "" {
			name = "kernel.run"
		}

		// Total cost = last cumulative cost row on the span.
		var cost float64
		if n := len(sp.Costs); n > 0 {
			cost = sp.Costs[n-1].TotalUSD
		}

		var (
			children     []otlpSpan
			firstPrompt  string
			lastOutput   string
			pendingTools = map[string]int{} // callID -> child span index
			n            int
			genCount     int // turn-completed generations emitted
		)

		for _, ev := range sp.Events {
			switch ev.Kind {
			case EventUserPromptSubmit:
				if p, ok := payloadOf[UserPromptPayload](ev); ok && firstPrompt == "" {
					firstPrompt = p.Prompt
				}

			case EventTurnCompleted:
				// One event now carries both the turn's content (what
				// EventAssistantTurn used to report) and its usage/cost (what
				// EventTurnCost used to report) — no accumulate-then-consume
				// dance across two events needed.
				p, ok := payloadOf[TurnPayload](ev)
				if !ok {
					continue
				}
				tv := summarizeTurn(p.Content)
				if tv.Text != "" {
					lastOutput = tv.Text
				}
				n++
				gen := otlpSpan{
					TraceID:           traceHex,
					SpanID:            childSpanID(rootHex, n),
					ParentSpanID:      rootHex,
					Name:              "llm.generation",
					Kind:              1,
					StartTimeUnixNano: strconv.FormatInt(ev.EmitTS, 10),
					EndTimeUnixNano:   strconv.FormatInt(ev.EmitTS, 10),
					Attributes: []otlpKeyValue{
						attr("langfuse.observation.type", "generation"),
						attr("gen_ai.request.model", sp.Model),
						jsonAttr("langfuse.observation.input", map[string]any{"role": "user", "content": firstPrompt}),
						jsonAttr("langfuse.observation.output", tv),
						jsonAttr("langfuse.observation.usage_details", usageDetails(p.TurnUsage)),
						jsonAttr("langfuse.observation.cost_details", map[string]any{"total": p.TurnCostUSD}),
					},
				}
				children = append(children, gen)
				genCount++

			case EventPreToolUse:
				p, ok := payloadOf[ToolUsePayload](ev)
				if !ok {
					continue
				}
				n++
				idx := len(children)
				children = append(children, otlpSpan{
					TraceID:           traceHex,
					SpanID:            childSpanID(rootHex, n),
					ParentSpanID:      rootHex,
					Name:              "tool." + p.Name,
					Kind:              1,
					StartTimeUnixNano: strconv.FormatInt(ev.EmitTS, 10),
					EndTimeUnixNano:   strconv.FormatInt(ev.EmitTS, 10),
					Attributes: []otlpKeyValue{
						attr("langfuse.observation.type", "tool"),
						jsonAttr("langfuse.observation.input", json.RawMessage(orJSON(p.Args))),
					},
				})
				if p.CallID != "" {
					pendingTools[p.CallID] = idx
				}

			case EventPostToolUse, EventPostToolUseFailure:
				p, ok := payloadOf[ToolUseResultPayload](ev)
				if !ok {
					continue
				}
				idx, found := pendingTools[p.CallID]
				if !found {
					continue
				}
				c := &children[idx]
				c.EndTimeUnixNano = strconv.FormatInt(ev.EmitTS, 10)
				out := p.Result
				if p.Error != "" {
					out = p.Error
					c.Attributes = append(c.Attributes, attr("langfuse.observation.level", "ERROR"))
				}
				c.Attributes = append(c.Attributes, attr("langfuse.observation.output", out))
				delete(pendingTools, p.CallID)

			case EventPostCompact:
				p, ok := payloadOf[CompactSummaryPayload](ev)
				if !ok {
					continue
				}
				n++
				children = append(children, otlpSpan{
					TraceID:           traceHex,
					SpanID:            childSpanID(rootHex, n),
					ParentSpanID:      rootHex,
					Name:              "compaction",
					Kind:              1,
					StartTimeUnixNano: strconv.FormatInt(ev.EmitTS, 10),
					EndTimeUnixNano:   strconv.FormatInt(ev.EmitTS, 10),
					Attributes: []otlpKeyValue{
						attr("langfuse.observation.type", "span"),
						// input = pre-compaction shape, output = what it collapsed to,
						// so the Langfuse input/output panes show the diff directly.
						jsonAttr("langfuse.observation.input", map[string]any{
							"messages": p.MessagesBefore, "tokens": p.TokensBefore,
						}),
						jsonAttr("langfuse.observation.output", map[string]any{
							"messages": p.MessagesAfter, "summary": p.Summary,
						}),
						jsonAttr("langfuse.observation.metadata", map[string]any{
							"messages_removed": p.MessagesBefore - p.MessagesAfter,
							"tokens_freed":     p.TokensBefore,
						}),
					},
				})

			default:
				// Everything else observable (subagent, notification, compact,
				// task, queue-interrupt …) becomes an event observation carrying
				// its raw payload, so nothing the kernel emits is dropped.
				if !ev.Kind.Observable() || ev.Payload == nil {
					continue
				}
				n++
				children = append(children, otlpSpan{
					TraceID:           traceHex,
					SpanID:            childSpanID(rootHex, n),
					ParentSpanID:      rootHex,
					Name:              string(ev.Kind),
					Kind:              1,
					StartTimeUnixNano: strconv.FormatInt(ev.EmitTS, 10),
					EndTimeUnixNano:   strconv.FormatInt(ev.EmitTS, 10),
					Attributes: []otlpKeyValue{
						attr("langfuse.observation.type", "event"),
						jsonAttr("langfuse.observation.output", ev.Payload),
					},
				})
			}
		}

		// Root span: carries the trace-level fields plus a span observation.
		root := otlpSpan{
			TraceID:           traceHex,
			SpanID:            rootHex,
			Name:              name,
			Kind:              1,
			StartTimeUnixNano: strconv.FormatInt(sp.StartedAt, 10),
			EndTimeUnixNano:   strconv.FormatInt(orNow(sp.EndedAt, sp.StartedAt), 10),
			Attributes: []otlpKeyValue{
				attr("langfuse.observation.type", "span"),
				attr("langfuse.session.id", sp.TraceID),
				attr("gen_ai.request.model", sp.Model),
			},
		}
		// Cost lives on the generations; only fall back to the root when a span
		// recorded cost but emitted no assistant turn (so the trace total is never
		// double-counted).
		if genCount == 0 && cost > 0 {
			root.Attributes = append(root.Attributes, jsonAttr("langfuse.observation.cost_details", map[string]any{"total": cost}))
		}
		// Every span shows its own I/O as an observation; the root additionally
		// sets the trace-level input/output and name.
		if firstPrompt != "" {
			root.Attributes = append(root.Attributes, jsonAttr("langfuse.observation.input", firstPrompt))
		}
		if lastOutput != "" {
			root.Attributes = append(root.Attributes, jsonAttr("langfuse.observation.output", lastOutput))
		}
		if sp.ParentSpanID != "" {
			_, pid := OTELIDs(sp.TraceID, sp.ParentSpanID)
			root.ParentSpanID = pid.String()
		} else {
			root.Attributes = append(root.Attributes, attr("langfuse.trace.name", name))
			if firstPrompt != "" {
				root.Attributes = append(root.Attributes, jsonAttr("langfuse.trace.input", firstPrompt))
			}
			if lastOutput != "" {
				root.Attributes = append(root.Attributes, jsonAttr("langfuse.trace.output", lastOutput))
			}
		}

		spans = append(spans, root)
		spans = append(spans, children...)
	}

	return otlpRequest{ResourceSpans: []otlpResourceSpans{{
		Resource:   otlpResource{Attributes: []otlpKeyValue{attr("service.name", "toroid-kernel")}},
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "toroid"}, Spans: spans}},
	}}}
}

// orJSON returns s if it is non-empty JSON, otherwise a JSON null, so tool args
// (already a JSON string) pass through without double-encoding.
func orJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "null"
	}
	return s
}

// orNow returns end if set, otherwise a 1ns-after-start fallback so spans always
// have a valid, ordered duration.
func orNow(end, start int64) int64 {
	if end > 0 {
		return end
	}
	return start + 1
}

// otlpMaxAttempts bounds the retry loop in postOTLP (1 try + 3 retries).
const otlpMaxAttempts = 4

// gzipJSON marshals req and gzip-compresses it. OTLP/JSON is verbose and highly
// compressible, so this typically shrinks the body several-fold.
func gzipJSON(req otlpRequest) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// postOTLP gzip-encodes and POSTs an OTLP request to an OTLP/HTTP JSON endpoint,
// retrying transient failures (transport errors, 429, and 5xx) with exponential
// backoff. 4xx responses fail immediately — retrying a rejected request is
// pointless. The context bounds the total wait.
func postOTLP(ctx context.Context, endpoint string, headers map[string]string, req otlpRequest) error {
	body, err := gzipJSON(req)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var lastErr error
	for attempt := 0; attempt < otlpMaxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 0.5s, 1s, 2s …
			delay := time.Duration(500<<(attempt-1)) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Content-Encoding", "gzip")
		for k, v := range headers {
			httpReq.Header.Set(k, v)
		}

		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = err // transport error: retry
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("OTLP export to %s failed: %s: %s", endpoint, resp.Status, msg)
			// transient: retry
		default:
			return fmt.Errorf("OTLP export to %s failed: %s: %s", endpoint, resp.Status, msg) // 4xx: give up
		}
	}
	return fmt.Errorf("OTLP export to %s failed after %d attempts: %w", endpoint, otlpMaxAttempts, lastErr)
}

// LangfuseOTLP pushes a stored trace to a Langfuse project's OTLP endpoint as a
// rich trace: input/output, model, token usage, cost, and one child observation
// per assistant turn and per tool call. The trace must have been recorded with
// Save:true. baseURL is the Langfuse host; publicKey/secretKey are project keys.
func LangfuseOTLP(ctx context.Context, traceID, baseURL, publicKey, secretKey string) error {
	td, err := LoadTraceData(traceID)
	if err != nil {
		return err
	}
	if len(td.Spans) == 0 {
		return fmt.Errorf("no spans found for trace %s (was the kernel created with Save:true?)", traceID)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/api/public/otel/v1/traces"
	auth := base64.StdEncoding.EncodeToString([]byte(publicKey + ":" + secretKey))
	return postOTLP(ctx, endpoint, map[string]string{"Authorization": "Basic " + auth}, buildLangfuseRequest(td))
}
