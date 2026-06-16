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

func TestNewGatewayProviderBuildsProvider(t *testing.T) {
	p, err := NewGatewayProvider("key", "https://gw.example.com/v1")
	if err != nil {
		t.Fatalf("NewGatewayProvider errored: %v", err)
	}
	if p == nil {
		t.Fatal("NewGatewayProvider returned nil provider")
	}
}

func TestNewProviderFromLLMIdUnknownProvider(t *testing.T) {
	if _, err := NewProviderFromLLMId("nope/some-model", "key"); err == nil {
		t.Fatal("expected an error for an unknown provider prefix")
	}
}

func TestNewProviderFromLLMIdGatewayNeedsBaseURL(t *testing.T) {
	t.Setenv(GatewayBaseURLEnv, "")
	_, err := NewProviderFromLLMId("llmgateway/claude-sonnet-4-6", "key")
	if err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("expected a base-URL error, got %v", err)
	}
}
