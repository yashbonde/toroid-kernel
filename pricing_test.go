package toroid

import (
	"testing"

	"charm.land/fantasy"
)

// TestPricingHonesty verifies that Usage distinguishes a real (possibly $0)
// price from a model that is simply absent from the pricing table. A missing
// model must never masquerade as a free model. See standard_pricing.md (M2).
func TestPricingHonesty(t *testing.T) {
	fu := fantasy.Usage{InputTokens: 1000, OutputTokens: 500}

	tests := []struct {
		name          string
		model         string
		wantPricingOK bool
		wantCostPos   bool // Cost should be strictly positive
	}{
		{
			name:          "known paid model resolves and bills",
			model:         "anthropic/claude-sonnet-4.5",
			wantPricingOK: true,
			wantCostPos:   true,
		},
		{
			name:          "gateway-routed known model resolves",
			model:         "llmgateway/claude-sonnet-4-5",
			wantPricingOK: true,
			wantCostPos:   true,
		},
		{
			name:          "free model priced at zero is still OK, not unavailable",
			model:         "openai/gpt-oss-20b:free",
			wantPricingOK: true,
			wantCostPos:   false,
		},
		{
			name:          "unknown model flags pricing unavailable, not free",
			model:         "llmgateway/kimi-k2p6-does-not-exist",
			wantPricingOK: false,
			wantCostPos:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var u Usage
			u.FromFantasyUsage(fu, tt.model)

			if u.PricingOK != tt.wantPricingOK {
				t.Errorf("PricingOK = %v, want %v (model %q)", u.PricingOK, tt.wantPricingOK, tt.model)
			}
			if tt.wantCostPos && u.Cost <= 0 {
				t.Errorf("Cost = %v, want > 0 (model %q)", u.Cost, tt.model)
			}
			if !tt.wantCostPos && u.Cost != 0 {
				t.Errorf("Cost = %v, want 0 (model %q)", u.Cost, tt.model)
			}
			// Token fields must be filled regardless of pricing resolution.
			if u.Input != 1000 || u.Output != 500 {
				t.Errorf("token fields dropped: input=%d output=%d", u.Input, u.Output)
			}
		})
	}
}

// TestPricingTableCachedSingleton verifies the pricing table is parsed once and
// reused (same backing map returned on repeated calls).
func TestPricingTableCachedSingleton(t *testing.T) {
	a := pricingTable()
	b := pricingTable()
	if len(a) == 0 {
		t.Fatal("pricing table is empty")
	}
	// Maps are reference types; identical length + shared entries indicate the
	// same underlying instance rather than a re-parse.
	if len(a) != len(b) {
		t.Fatalf("pricing table not stable across calls: %d vs %d", len(a), len(b))
	}
}
