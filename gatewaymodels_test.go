package toroid

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchGatewayModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gateway-secret" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"model-b","context_length":200000,"pricing":{"input":1400000,"output":4400000}},{"id":" model-a ","max_input_tokens":1048576,"context_length":200000},{"id":""}]}`)
	}))
	defer server.Close()

	models, err := FetchGatewayModels(context.Background(), server.Client(), server.URL+"/v1/", "gateway-secret")
	if err != nil {
		t.Fatalf("FetchGatewayModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v, want 2 entries", models)
	}
	if got := models[0]; got.ID != "model-b" || got.ContextLength != 200000 || got.Pricing == nil || got.Pricing.Input != 1400000 || got.Pricing.Output != 4400000 {
		t.Fatalf("first model = %#v, want model-b with pricing", got)
	}
	if got := models[1]; got.ID != "model-a" || got.ContextLength != 1048576 || got.Pricing != nil {
		t.Fatalf("second model = %#v, want trimmed unpriced model-a", got)
	}
}

func TestFetchGatewayModelsReportsGatewayError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid key", http.StatusUnauthorized)
	}))
	defer server.Close()

	if _, err := FetchGatewayModels(context.Background(), server.Client(), server.URL, "bad-key"); err == nil || err.Error() != "gateway returned 401 Unauthorized: invalid key" {
		t.Fatalf("FetchGatewayModels error = %v, want gateway status and message", err)
	}
}

func TestGatewayModelsPromptUsesLiveCatalog(t *testing.T) {
	models := []GatewayModel{
		{ID: "expensive", Pricing: &GatewayModelPricing{Input: 4_500_000, Output: 22_500_000}},
		{ID: "unpriced"},
		{ID: "cheap-b", Pricing: &GatewayModelPricing{Input: 200_000, Output: 1_200_000}},
		{ID: "cheap-a", Pricing: &GatewayModelPricing{Input: 200_000, Output: 1_200_000}},
	}

	got := gatewayModelsPrompt(models)
	if !strings.Contains(got, "live catalog; cheapest first") {
		t.Fatalf("prompt = %q, want live catalog label", got)
	}
	if strings.Contains(got, "unpriced") {
		t.Fatalf("prompt = %q, must omit models without gateway pricing", got)
	}
	if want := "cheap-a  $0.20 / $1.20\ncheap-b  $0.20 / $1.20\nexpensive  $4.50 / $22.50"; !strings.Contains(got, want) {
		t.Fatalf("prompt = %q, want sorted live pricing %q", got, want)
	}
}

func TestGatewayModelsPromptBoundsLargeCatalog(t *testing.T) {
	models := make([]GatewayModel, maxGatewayModelsInPrompt+1)
	for i := range models {
		models[i] = GatewayModel{
			ID:      fmt.Sprintf("model-%02d", i),
			Pricing: &GatewayModelPricing{Input: float64(i + 1)},
		}
	}

	got := gatewayModelsPrompt(models)
	if strings.Contains(got, "model-10") {
		t.Fatalf("prompt includes more than %d models: %q", maxGatewayModelsInPrompt, got)
	}
	if !strings.Contains(got, "… and 1 more; run `trk models` for the full catalog.") {
		t.Fatalf("prompt = %q, want truncation note", got)
	}
}
