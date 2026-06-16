package toroid

import (
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openaicompat"
)

// GatewayBaseURLEnv is the primary environment variable holding the base URL of
// an OpenAI-compatible LLM gateway (e.g. a self-hosted LiteLLM proxy). The value
// is the gateway origin; the provider appends the OpenAI "/v1" path segment if
// it is not already present. GatewayBaseURLEnvAlt is accepted as a fallback.
const (
	GatewayBaseURLEnv    = "LLM_GATEWAY_BASE_URL"
	GatewayBaseURLEnvAlt = "LLM_GATEWAY_BASE"
)

// gatewayBaseURL resolves the configured gateway base URL from the environment.
func gatewayBaseURL() string {
	if v := os.Getenv(GatewayBaseURLEnv); v != "" {
		return v
	}
	return os.Getenv(GatewayBaseURLEnvAlt)
}

// NewProviderFromLLMId creates a fantasy.Provider from an LLM ID of the form
// "provider/model-name" (matching the keys in pricing.json).
// Supported prefixes: google, anthropic, openai, llmgateway.
func NewProviderFromLLMId(llmID, apiKey string) (fantasy.Provider, error) {
	provider, _, _ := strings.Cut(llmID, "/")
	switch provider {
	case "google", "":
		return google.New(google.WithGeminiAPIKey(apiKey))
	case "anthropic":
		return anthropic.New(anthropic.WithAPIKey(apiKey))
	case "openai":
		return openaicompat.New(openaicompat.WithAPIKey(apiKey))
	case "llmgateway":
		return NewGatewayProvider(apiKey, gatewayBaseURL())
	default:
		return nil, fmt.Errorf("unknown provider %q in LLM ID %q", provider, llmID)
	}
}

// NewGatewayProvider builds a provider for an OpenAI-compatible LLM gateway
// (e.g. a LiteLLM proxy that fronts many model families behind one endpoint).
// The gateway speaks the OpenAI `/v1/chat/completions` protocol — including
// streaming and tool calls — and authenticates with a bearer token, so this is
// a thin configuration of the openai-compatible provider rather than a bespoke
// transport.
//
// baseURL is the gateway origin (e.g. "https://my-gateway.example.com"). The
// OpenAI "/v1" path segment is appended automatically when absent. apiKey is
// sent as the bearer token.
func NewGatewayProvider(apiKey, baseURL string) (fantasy.Provider, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("llmgateway provider requires a base URL (set %s or %s)", GatewayBaseURLEnv, GatewayBaseURLEnvAlt)
	}
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	return openaicompat.New(
		openaicompat.WithName("llmgateway"),
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	)
}
