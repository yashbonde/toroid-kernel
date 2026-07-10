package toroid

// Usage is the token + dollar accounting for one llm-step (or a rollup of
// them). Cost comes exclusively from the gateway: LiteLLM reports the
// authoritative per-call cost in the x-litellm-response-cost header on
// non-streaming responses — there is no local pricing table. PricingOK is true
// only when the gateway reported a cost; a false value means "cost unknown",
// never "free".
type Usage struct {
	Output     int64
	Input      int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
	PricingOK  bool
}
