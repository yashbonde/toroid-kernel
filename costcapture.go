package toroid

import (
	"context"
	"strconv"
	"sync"
)

// Gateway cost capture (Phase C, hybrid strategy from standard_pricing.md §5).
//
// LiteLLM computes real dollar cost only after token counts are known. For a
// NON-streaming response it can put that in the `x-litellm-response-cost`
// header before the headers are sent; for a STREAMING response the headers go
// out first, so the cost header is absent. So the gateway's authoritative cost
// is available for Complete / CompleteObject (compact, structured output) but
// not for the streaming agent loop — which correctly stays on the local
// pricing.json estimate.
//
// The mechanism is a per-call sink carried in the request context: a Step wraps
// the context with a sink before a non-streaming call, the gateway transport
// writes any captured cost into it, and the Step prefers that gateway truth over
// the local estimate when present.

// costSinkKey carries a *costSink through a request context.
type costSinkKey struct{}

// costSink receives the gateway-reported cost for one non-streaming llm-step.
type costSink struct {
	mu      sync.Mutex
	cost    float64
	present bool
}

func (s *costSink) set(cost float64) {
	s.mu.Lock()
	s.cost = cost
	s.present = true
	s.mu.Unlock()
}

// read returns the captured cost and whether the gateway reported one.
func (s *costSink) read() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cost, s.present
}

// withCostSink attaches a fresh cost sink to ctx for a single non-streaming
// gateway call and returns it so the caller can read the gateway cost afterward.
func withCostSink(ctx context.Context) (context.Context, *costSink) {
	s := &costSink{}
	return context.WithValue(ctx, costSinkKey{}, s), s
}

// costSinkFromContext returns the sink attached to ctx, if any.
func costSinkFromContext(ctx context.Context) *costSink {
	s, _ := ctx.Value(costSinkKey{}).(*costSink)
	return s
}

// litellmResponseCostHeader is LiteLLM's per-call cost header (non-streaming).
const litellmResponseCostHeader = "x-litellm-response-cost"

// captureGatewayCost parses the LiteLLM cost header and, if a sink is present in
// ctx and the value is meaningful, stores it. Called from the gateway transport
// on each response. A missing/empty/zero header is ignored so streaming calls
// (which omit or zero the header) fall through to the local estimate.
func captureGatewayCost(ctx context.Context, headerValue string) {
	if headerValue == "" {
		return
	}
	sink := costSinkFromContext(ctx)
	if sink == nil {
		return
	}
	cost, err := strconv.ParseFloat(headerValue, 64)
	if err != nil || cost <= 0 {
		return
	}
	sink.set(cost)
}

// applyGatewayCost overrides a locally-estimated Usage with the gateway's
// reported cost when one was captured for this call. Gateway truth beats the
// pricing.json estimate; when a real cost lands, PricingOK is necessarily true.
func applyGatewayCost(ctx context.Context, u *Usage) {
	sink := costSinkFromContext(ctx)
	if sink == nil {
		return
	}
	if cost, ok := sink.read(); ok {
		u.Cost = cost
		u.PricingOK = true
	}
}
