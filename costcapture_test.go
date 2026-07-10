package toroid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// roundTrip drives a request through traceTransport against a stub server that
// echoes the given cost header, and returns the Usage after applyGatewayCost.
func captureThrough(t *testing.T, ctx context.Context, costHeader string) Usage {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if costHeader != "" {
			w.Header().Set(litellmResponseCostHeader, costHeader)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	tr := &traceTransport{base: http.DefaultTransport}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Start from a local estimate as FromFantasyUsage would produce, then apply.
	u := Usage{Input: 1000, Output: 100, Cost: 0.5, PricingOK: true}
	applyGatewayCost(ctx, &u)
	return u
}

// TestGatewayCostPreferredOnNonStream verifies the gateway's reported cost
// overrides the local estimate when the header is present (Phase C, Option A).
func TestGatewayCostPreferredOnNonStream(t *testing.T) {
	ctx, _ := withCostSink(context.Background())
	u := captureThrough(t, ctx, "0.012345")
	if u.Cost != 0.012345 {
		t.Errorf("Cost = %v, want gateway-reported 0.012345", u.Cost)
	}
	if !u.PricingOK {
		t.Error("PricingOK should be true when a real gateway cost lands")
	}
}

// TestGatewayCostAbsentKeepsEstimate verifies that with no cost header (the
// streaming case) the local estimate is left untouched.
func TestGatewayCostAbsentKeepsEstimate(t *testing.T) {
	ctx, _ := withCostSink(context.Background())
	u := captureThrough(t, ctx, "") // header absent, as on streaming responses
	if u.Cost != 0.5 {
		t.Errorf("Cost = %v, want local estimate 0.5 preserved", u.Cost)
	}
}

// TestGatewayCostZeroIgnored verifies a 0.0 header (LiteLLM's streaming
// placeholder) does not clobber a real local estimate.
func TestGatewayCostZeroIgnored(t *testing.T) {
	ctx, _ := withCostSink(context.Background())
	u := captureThrough(t, ctx, "0.0")
	if u.Cost != 0.5 {
		t.Errorf("Cost = %v, want local estimate preserved when header is 0.0", u.Cost)
	}
}

// TestNoSinkNoPanic verifies a request without a sink in context is a safe no-op.
func TestNoSinkNoPanic(t *testing.T) {
	u := captureThrough(t, context.Background(), "0.99")
	if u.Cost != 0.5 {
		t.Errorf("Cost = %v, want estimate preserved when no sink attached", u.Cost)
	}
}
