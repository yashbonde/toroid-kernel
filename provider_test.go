package toroid

import (
	"strings"
	"testing"
)

func TestNewGatewayProviderRequiresBaseURL(t *testing.T) {
	if _, err := NewGatewayProvider("key", "  "); err == nil {
		t.Fatal("expected an error when the gateway base URL is empty")
	}
}

func TestNewGatewayProviderNormalizesBaseURL(t *testing.T) {
	// Each input should resolve to a working provider regardless of trailing
	// slash or an already-present /v1 segment.
	for _, in := range []string{
		"https://gw.example.com",
		"https://gw.example.com/",
		"https://gw.example.com/v1",
		"https://gw.example.com/v1/",
	} {
		p, err := NewGatewayProvider("key", in)
		if err != nil {
			t.Fatalf("NewGatewayProvider(%q) errored: %v", in, err)
		}
		if p == nil {
			t.Fatalf("NewGatewayProvider(%q) returned nil provider", in)
		}
	}
}

func TestGatewayBaseURLPrefersPrimaryEnv(t *testing.T) {
	t.Setenv(GatewayBaseURLEnv, "https://primary.example.com")
	t.Setenv(GatewayBaseURLEnvAlt, "https://alt.example.com")
	if got := gatewayBaseURL(); got != "https://primary.example.com" {
		t.Fatalf("gatewayBaseURL() = %q, want primary", got)
	}
}

func TestGatewayBaseURLFallsBackToAltEnv(t *testing.T) {
	t.Setenv(GatewayBaseURLEnv, "")
	t.Setenv(GatewayBaseURLEnvAlt, "https://alt.example.com")
	if got := gatewayBaseURL(); got != "https://alt.example.com" {
		t.Fatalf("gatewayBaseURL() = %q, want alt fallback", got)
	}
}

func TestNewProviderFromLLMIdUnknownProvider(t *testing.T) {
	if _, err := NewProviderFromLLMId("nope/some-model", "key"); err == nil {
		t.Fatal("expected an error for an unknown provider prefix")
	}
}

func TestNewProviderFromLLMIdGatewayNeedsBaseURL(t *testing.T) {
	t.Setenv(GatewayBaseURLEnv, "")
	t.Setenv(GatewayBaseURLEnvAlt, "")
	_, err := NewProviderFromLLMId("llmgateway/claude-sonnet-4-6", "key")
	if err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("expected a base-URL error, got %v", err)
	}
}
