package toroid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/openai-go/option"
)

// gatewayUserAgent identifies toroid-kernel as the caller on every request to
// the LLM gateway, so the upstream (and its Langfuse hook) can attribute traffic.
const gatewayUserAgent = "toroid-kernel"

// chatTraceKey carries a per-chat (per kernel.Run) W3C trace id through the
// request context so the gateway transport can stamp a consistent traceparent on
// every turn — grouping all of a chat's LLM calls under one upstream trace.
type chatTraceKey struct{}

// WithGatewayTrace returns a context carrying a stable 16-byte (32-hex) trace id
// for one chat. If the context already carries one (e.g. a subagent running
// inside a parent chat), it is preserved so nested calls stay in the same trace.
func WithGatewayTrace(ctx context.Context) context.Context {
	if v, ok := ctx.Value(chatTraceKey{}).(string); ok && v != "" {
		return ctx
	}
	return context.WithValue(ctx, chatTraceKey{}, randomHex(16))
}

func gatewayTraceID(ctx context.Context) string {
	if v, ok := ctx.Value(chatTraceKey{}).(string); ok {
		return v
	}
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// traceTransport stamps a W3C traceparent (stable trace id from the request
// context + a fresh span id per request) plus a matching x-litellm-session-id on
// every outgoing gateway request, so all turns of one chat nest under a single
// upstream/Langfuse trace.
type traceTransport struct{ base http.RoundTripper }

func (t *traceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	traceID := gatewayTraceID(req.Context())
	if traceID == "" {
		return base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	req.Header.Set("traceparent", "00-"+traceID+"-"+randomHex(8)+"-01")
	req.Header.Set("x-litellm-session-id", traceID)
	return base.RoundTrip(req)
}

// GatewayBaseURLEnv is the environment variable holding the OpenAI-compatible
// base URL of an LLM gateway (e.g. a self-hosted LiteLLM proxy), including the
// "/v1" segment — e.g. "https://my-gateway.example.com/v1".
const GatewayBaseURLEnv = "LLM_GATEWAY_BASE_URL"

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
		return NewGatewayProvider(apiKey, os.Getenv(GatewayBaseURLEnv))
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
// baseURL is the gateway's OpenAI-compatible base, including the "/v1" segment
// (e.g. "https://my-gateway.example.com/v1"). apiKey is sent as the bearer token.
func NewGatewayProvider(apiKey, baseURL string) (fantasy.Provider, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("llmgateway provider requires a base URL (set %s)", GatewayBaseURLEnv)
	}
	return openaicompat.New(
		openaicompat.WithName("llmgateway"),
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
		// Stamp a stable User-Agent on every gateway request (client-level, so it
		// covers the main agent, subagents, and structured-generation calls).
		openaicompat.WithSDKOptions(option.WithHeader("User-Agent", gatewayUserAgent)),
		// Group all turns of one chat under a single upstream trace via traceparent.
		openaicompat.WithHTTPClient(&http.Client{Transport: &traceTransport{base: http.DefaultTransport}}),
	)
}
