package toroid

// Usage is the token + dollar accounting for one llm-step (or a rollup of
// them). Cost comes from the gateway's cost header when present.
// PricingOK is true only when a cost was reported; a false value means
// "cost unknown", never "free".
type Usage struct {
	Output     int64
	Input      int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
	PricingOK  bool
}
