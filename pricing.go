package toroid

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"charm.land/fantasy"
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

	// Model pricing lookup
	id := strings.ToLower(modelID)
	if strings.HasPrefix(id, "models/") {
		id = id[7:]
	}
	// Normalize: try both the raw id and a dot-substituted variant (e.g. haiku-4-5 -> haiku-4.5)
	candidates := []string{id, strings.NewReplacer("-4-5", "-4.5", "-3-5", "-3.5", "-2-0", "-2.0").Replace(id)}
	for _, candidate := range candidates {
		if pricing, ok := p.table[candidate]; ok {
			return pricing, nil // Direct match
		}
		if !strings.Contains(candidate, "/") {
			if pricing, ok := p.table["google/"+candidate]; ok {
				return pricing, nil // Match with google/ prefix
			}
		}
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
