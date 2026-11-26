package toroid

// Usage is the token + dollar accounting for one llm-step (or a rollup of
// them). A priced step records whether its cost was reported by the gateway
// or estimated from token usage at the gateway catalog rates.
type Usage struct {
	Output     int64
	Input      int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
	PricingOK  bool
	CostSource string `json:"cost_source,omitempty"`
}
