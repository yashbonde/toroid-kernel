package toroid

import "testing"

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name         string
		id           string
		wantProvider string
		wantAPI      string
		wantPricing  bool
		wantImage    bool
		wantWindow   int
	}{
		{
			name:         "gateway claude resolves to openai-completions with pricing and vision",
			id:           "llmgateway/claude-sonnet-4-5",
			wantProvider: "llmgateway",
			wantAPI:      APIOpenAICompletions,
			wantPricing:  true,
			wantImage:    true,
			wantWindow:   200_000,
		},
		{
			name:         "fully unknown gateway model: usable, pricing unavailable, text-only, window 0",
			id:           "llmgateway/foobar-9000",
			wantProvider: "llmgateway",
			wantAPI:      APIOpenAICompletions,
			wantPricing:  false,
			wantImage:    false,
			wantWindow:   0,
		},
		{
			name:         "known text-only family (kimi) gets a context window via family fallback, no pricing",
			id:           "llmgateway/kimi-k2p6",
			wantProvider: "llmgateway",
			wantAPI:      APIOpenAICompletions,
			wantPricing:  false, // absent from pricing.json — honest unavailable
			wantImage:    false, // text-only base
			wantWindow:   128_000,
		},
		{
			name:         "native anthropic id maps to anthropic-messages api",
			id:           "anthropic/claude-opus-4.8",
			wantProvider: "anthropic",
			wantAPI:      APIAnthropicMessages,
			wantPricing:  true,
			wantImage:    true,
			wantWindow:   200_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ResolveModel(tt.id)
			if m.Provider != tt.wantProvider {
				t.Errorf("Provider = %q, want %q", m.Provider, tt.wantProvider)
			}
			if m.API != tt.wantAPI {
				t.Errorf("API = %q, want %q", m.API, tt.wantAPI)
			}
			if m.PricingOK != tt.wantPricing {
				t.Errorf("PricingOK = %v, want %v", m.PricingOK, tt.wantPricing)
			}
			if m.SupportsImage() != tt.wantImage {
				t.Errorf("SupportsImage() = %v, want %v", m.SupportsImage(), tt.wantImage)
			}
			if m.ContextWindow != tt.wantWindow {
				t.Errorf("ContextWindow = %d, want %d", m.ContextWindow, tt.wantWindow)
			}
			// Every resolved model must at least accept text.
			if len(m.Input) == 0 || m.Input[0] != InputText {
				t.Errorf("Input = %v, want text first", m.Input)
			}
		})
	}
}

// TestResolveModelReasoningFromPricing verifies reasoning capability is inferred
// from a non-zero reasoning rate even without a metadata-table entry.
func TestResolveModelReasoningFromPricing(t *testing.T) {
	// google/gemini-2.5-flash-lite has a non-zero Reasoning rate in pricing.json
	// but no modelMetaTable row.
	m := ResolveModel("google/gemini-2.5-flash-lite")
	if !m.PricingOK {
		t.Fatal("expected gemini-2.5-flash-lite to be priced")
	}
	if !m.Reasoning {
		t.Error("expected Reasoning=true inferred from non-zero reasoning rate")
	}
}
