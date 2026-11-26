package llm

import (
	"net/http"
	"testing"
)

func TestGatewayCostUnitsAndStreamingReservations(t *testing.T) {
	h := make(http.Header)
	h.Set("X-LLM-Gateway-Cost", "12500")
	got := gatewayInfoFrom(h, false)
	if got.CostUSD == nil || *got.CostUSD != .0125 || got.Reserved {
		t.Fatalf("nonstream micro-USD: %+v", got)
	}
	got = gatewayInfoFrom(h, true)
	if got.CostUSD != nil || !got.Reserved {
		t.Fatalf("stream reservation: %+v", got)
	}
	h.Set("x-litellm-response-cost", "0.00017")
	got = gatewayInfoFrom(h, true)
	if got.CostUSD == nil || *got.CostUSD != .00017 {
		t.Fatalf("stream reported USD: %+v", got)
	}
	h.Del("x-litellm-response-cost")
	h.Set("X-LLM-Gateway-Cost", "0")
	got = gatewayInfoFrom(h, false)
	if got.CostUSD == nil || *got.CostUSD != 0 {
		t.Fatal("missing explicit free price")
	}
}
