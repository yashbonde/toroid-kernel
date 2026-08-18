package toroid

import (
	"context"

	"github.com/yashbonde/toroid-kernel/llm"
)

// GatewayBaseURLEnv is the environment variable holding the OpenAI-compatible
// base URL of the LLM gateway (a LiteLLM proxy), including the "/v1" segment —
// e.g. "https://my-gateway.example.com/v1".
const GatewayBaseURLEnv = "LLM_GATEWAY_BASE_URL"

// GatewayKeyEnv is the environment variable holding the gateway bearer token,
// used when Config.APIKey is empty.
const GatewayKeyEnv = "LLM_GATEWAY_KEY"

// Direct provider routes: an "openai/<model>" id talks straight to the OpenAI
// API (same OpenAI-compatible wire); an "anthropic/<model>" id talks to the
// native Anthropic messages API. Cost on all routes is priced from the
// catalog's cached rates (Model.Price).
const (
	OpenAIBaseURL   = "https://api.openai.com/v1"
	OpenAIKeyEnv    = "OPENAI_API_KEY"
	AnthropicKeyEnv = "ANTHROPIC_API_KEY"
)

// WithGatewayTrace returns a context carrying a stable 16-byte (32-hex) trace id
// for one chat, so the llm client stamps a consistent traceparent (plus a
// matching x-litellm-session-id) on every llm-step — grouping all of a chat's
// LLM calls under one upstream/Langfuse trace. If the context already carries
// one (e.g. a subagent running inside a parent chat), it is preserved so nested
// calls stay in the same trace.
func WithGatewayTrace(ctx context.Context) context.Context {
	return llm.WithSessionTrace(ctx)
}
