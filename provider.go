package toroid

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openaicompat"
)

// NewProviderFromLLMId creates a fantasy.Provider from an LLM ID of the form
// "provider/model-name" (matching the keys in pricing.json).
// Supported prefixes: google, anthropic, openai.
func NewProviderFromLLMId(llmID, apiKey string) (fantasy.Provider, error) {
	provider, _, _ := strings.Cut(llmID, "/")
	switch provider {
	case "google", "":
		return google.New(google.WithGeminiAPIKey(apiKey))
	case "anthropic":
		return anthropic.New(anthropic.WithAPIKey(apiKey))
	case "openai":
		return openaicompat.New(openaicompat.WithAPIKey(apiKey))
	default:
		return nil, fmt.Errorf("unknown provider %q in LLM ID %q", provider, llmID)
	}
}
