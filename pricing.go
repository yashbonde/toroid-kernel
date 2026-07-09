package toroid

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"charm.land/fantasy"
)

// versionDotReplacer normalizes dash-separated version suffixes in model IDs
// to the dotted form used in pricing.json (e.g. "claude-sonnet-4-6" ->
// "claude-sonnet-4.6"). It is intentionally conservative — only literal
// single-digit/single-digit pairs that actually appear in the table are
// rewritten, so param-size suffixes like "gemma-4-26b" are left untouched.
var versionDotReplacer = strings.NewReplacer(
	"-4-8", "-4.8", "-4-7", "-4.7", "-4-6", "-4.6", "-4-5", "-4.5", "-4-1", "-4.1",
	"-3-5", "-3.5", "-3-1", "-3.1",
	"-2-5", "-2.5", "-2-0", "-2.0",
	"-5-5", "-5.5", "-5-4", "-5.4", "-5-3", "-5.3", "-5-2", "-5.2", "-5-1", "-5.1",
)

// ModelPricing defines the cost per token for an LLM.
type ModelPricing struct {
	Prompt     float64 `json:"Prompt"`
	Completion float64 `json:"Completion"`
	Reasoning  float64 `json:"Reasoning"`
	CacheRead  float64 `json:"CacheRead"`
	CacheWrite float64 `json:"CacheWrite"`
}

// LLM endpoint returns cost in USD, for local currency calculation
// we need to create a map
type USDToLocalCurrency struct {
	Name string  `json:"name"`
	Rate float64 `json:"rate"`
	Date string  `json:"date"`
}

// GetModelPricing returns the singleton pricing instance.
func GetModelPricing(modelID string) (ModelPricing, error) {
	type Pricing struct {
		table map[string]ModelPricing
	}

	p := &Pricing{
		table: make(map[string]ModelPricing),
	}
	data, err := readAssets("pricing.json")
	if err != nil {
		log.Fatal("Failed to read pricing.json: ", err)
	}
	if err := json.Unmarshal(data, &p.table); err != nil {
		log.Fatal("Failed to unmarshal pricing.json: ", err)
	}

	// Model pricing lookup.
	id := strings.ToLower(modelID)
	id = strings.TrimPrefix(id, "models/")
	// "llmgateway/" is a virtual OpenAI-compatible provider that fronts real
	// models behind a single endpoint; strip it so the underlying model name
	// resolves to its real pricing entry (e.g. llmgateway/claude-sonnet-4-6 ->
	// anthropic/claude-sonnet-4.6).
	id = strings.TrimPrefix(id, "llmgateway/")

	// Normalize version suffixes: try the raw id and a dot-substituted variant
	// (e.g. haiku-4-5 -> haiku-4.5).
	dotted := versionDotReplacer.Replace(id)
	nameVariants := []string{id}
	if dotted != id {
		nameVariants = append(nameVariants, dotted)
	}

	// Try each name variant against the bare key and each known provider prefix.
	// This lets a gateway-routed bare name (e.g. "claude-sonnet-4.6") match its
	// "anthropic/claude-sonnet-4.6" entry, and a bare Google model match
	// "google/...". Exact matches across all variants are preferred over the
	// fuzzy fallback below.
	providerPrefixes := []string{"", "anthropic/", "openai/", "google/"}
	for _, name := range nameVariants {
		hasProvider := strings.Contains(name, "/")
		for _, pfx := range providerPrefixes {
			if hasProvider && pfx != "" {
				continue // don't double-prefix "anthropic/anthropic/..."
			}
			if pricing, ok := p.table[pfx+name]; ok {
				return pricing, nil
			}
		}
	}

	// Last-resort fuzzy prefix match: a minor revision absent from the table
	// falls back to its base model (e.g. "claude-opus-4.8.1" -> "claude-opus-4.8").
	// Kept for backwards compatibility.
	for _, candidate := range nameVariants {
		for k, pricing := range p.table {
			if strings.HasPrefix(candidate, k) || strings.HasPrefix(k, candidate) {
				return pricing, nil
			}
		}
	}
	return ModelPricing{}, fmt.Errorf("model %s not found in pricing table", modelID)
}

func GetCurrencyMultiplier(curr string) (USDToLocalCurrency, error) {
	curr = strings.ToUpper(curr)
	if curr == "" || curr == "USD" {
		return USDToLocalCurrency{
			Name: "US Dollar",
			Rate: 1,
			Date: "base - 1",
		}, nil
	}

	type Currency struct {
		table map[string]USDToLocalCurrency
	}
	p := &Currency{
		table: make(map[string]USDToLocalCurrency),
	}
	data, err := readAssets("usd_x.json")
	if err != nil {
		log.Fatal("Failed to read usd_x.json: ", err)
	}
	if err := json.Unmarshal(data, &p.table); err != nil {
		log.Fatal("Failed to unmarshal usd_x.json: ", err)
	}
	if pricing, ok := p.table[strings.ToLower(curr)]; ok {
		return pricing, nil // Direct match (keys in usd_x.json are lowercase)
	}
	return USDToLocalCurrency{
		Name: "Unknown Currency",
		Rate: 0,
		Date: "unknown",
	}, fmt.Errorf("Unknown Currency %s", curr)
}

// CalculateCost computes the total cost for a usage breakdown using default pricing.
func CalculateCost(modelID string, usage Usage, curr string) (float64, error) {
	p, err := GetModelPricing(modelID)
	if err != nil {
		return 0, err
	}
	m, err := GetCurrencyMultiplier(curr)
	if err != nil {
		return 0, err
	}

	// In most modern APIs (Gemini, OpenAI O1/O3), OutputTokens include ReasoningTokens.
	// We subtract reasoning to avoid double-charging.
	contentTokens := float64(usage.Output - usage.Reasoning)
	if contentTokens < 0 {
		contentTokens = 0
	}

	return (float64(usage.Input)*p.Prompt +
		contentTokens*p.Completion +
		float64(usage.Reasoning)*p.Reasoning +
		float64(usage.CacheRead)*p.CacheRead +
		float64(usage.CacheWrite)*p.CacheWrite) * m.Rate, nil
}

// Usage tracker

type Usage struct {
	Output     int64
	Input      int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
}

func (u *Usage) FromFantasyUsage(usage fantasy.Usage, model string) {
	u.Output = usage.OutputTokens
	u.Input = usage.InputTokens
	u.Reasoning = usage.ReasoningTokens
	u.CacheRead = usage.CacheReadTokens
	u.CacheWrite = usage.CacheCreationTokens
	u.Cost, _ = CalculateCost(model, *u, "USD") // here we can ignore error because string is hardcoded
}
