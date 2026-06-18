package toroid

import (
	"strings"
	"testing"
)

// findAttr returns the string value of the first span attribute with key on the
// span named name.
func findAttr(t *testing.T, req otlpRequest, spanName, key string) (string, bool) {
	t.Helper()
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				if sp.Name != spanName {
					continue
				}
				for _, a := range sp.Attributes {
					if a.Key == key {
						s, _ := a.Value["stringValue"].(string)
						return s, true
					}
				}
			}
		}
	}
	return "", false
}

// TestLangfuseImageRefStaysPath verifies that an inline image ref in the prompt
// is exported verbatim as a ![](path) reference — the image is NOT embedded as
// bytes, keeping traces light.
func TestLangfuseImageRefStaysPath(t *testing.T) {
	now := int64(1_700_000_000_000_000_000)
	prompt := "Describe this image? ![](exmaples/img.jpg)"
	td := TraceData{
		Trace: TraceMeta{TraceID: "t", Title: "describe-image", StartedAt: now},
		Spans: []SpanData{{
			SpanMeta: SpanMeta{SpanID: "t", TraceID: "t", Title: "describe-image", Model: "llmgateway/kimi-k2p6", StartedAt: now, EndedAt: now + 1},
			Events: []Event{
				{Kind: EventUserPromptSubmit, EmitTS: now, Seq: 1, Payload: UserPromptPayload{Prompt: prompt}},
			},
		}},
	}

	ref := string(imageRefRe.Find([]byte(prompt))) // the ![](…) ref, taken from the prompt
	if ref == "" {
		t.Fatal("test prompt must contain an image ref")
	}

	req := buildLangfuseRequest(td)
	in, ok := findAttr(t, req, "describe-image", "langfuse.trace.input")
	if !ok {
		t.Fatal("trace input attribute missing")
	}
	if !strings.Contains(in, ref) {
		t.Fatalf("expected image ref %q preserved verbatim, got %q", ref, in)
	}
	if strings.Contains(in, "base64") || strings.Contains(in, "data:image") {
		t.Fatalf("image bytes should NOT be embedded, got %q", in)
	}
}
