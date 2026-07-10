package toroid

import "strings"

// Model as data (M1). A catalog row describing one runnable model: how to reach
// it (provider + wire api), its limits (context window, input modalities), and
// its cost. This is the "model as data" contract the LLM-step layer will consume
// so cost and capability decisions are made from a table, never guessed inline.
//
// Only the OpenAI-compatible LiteLLM gateway ("llmgateway/*", api
// "openai-completions") is in scope for this port; native Anthropic/Google/
// OpenAI rows may resolve but are not a supported target here. See
// assets/llm-step-port-scope.md.
type Model struct {
	// ID is the full model id as the host uses it, e.g. "llmgateway/kimi-k2p6".
	ID string
	// Provider is the id prefix before the first "/", e.g. "llmgateway".
	Provider string
	// API is the wire protocol used to reach the model.
	API string
	// ContextWindow is the model's max context in tokens, or 0 when unknown.
	ContextWindow int
	// Cost holds per-token USD rates. Meaningful only when PricingOK is true.
	Cost ModelPricing
	// PricingOK reports whether Cost came from a real pricing.json row. When
	// false the model is absent from the table and Cost must be treated as
	// "unavailable", never as free. Mirrors Usage.PricingOK (M2).
	PricingOK bool
	// Reasoning reports whether the model emits (and bills) reasoning tokens.
	Reasoning bool
	// Input lists supported input modalities, e.g. ["text"] or ["text","image"].
	Input []string
}

// Wire API identifiers.
const (
	APIOpenAICompletions = "openai-completions" // LiteLLM / llmgateway (in scope)
	APIAnthropicMessages = "anthropic-messages" // out of scope for this port
	APIGoogle            = "google"             // out of scope for this port
)

// Input modality identifiers.
const (
	InputText  = "text"
	InputImage = "image"
)

// modelMeta carries the non-pricing facts about a model that pricing.json does
// not encode (context window, vision, reasoning). It is intentionally small and
// explicit: we seed only what we can state truthfully and default the rest,
// rather than guessing capabilities from the name.
type modelMeta struct {
	contextWindow int
	vision        bool
	reasoning     bool
}

// modelMetaTable maps a normalized (provider-stripped, dotted) model name to its
// known metadata, for exact overrides. Keys use the same bare form
// GetModelPricing resolves, so a gateway id like "llmgateway/claude-sonnet-4-5"
// and a bare "claude-sonnet-4.5" share one entry. An exact hit wins over the
// family fallback below.
var modelMetaTable = map[string]modelMeta{
	// Reserved for per-id overrides where a specific version departs from its
	// family defaults. Empty by default — most models resolve via the families.
}

// modelMetaFamily matches a model name by prefix so every version of a known
// family inherits sensible metadata without enumerating each dotted release
// (e.g. all "claude-sonnet-*" get a 200K vision window). Ordered most-specific
// prefix first; the first prefix that the normalized name starts with wins.
//
// Context windows are the published maximums for the family and are informational
// (the kernel's compaction threshold uses Config.TotalContextSize, not this).
// Vision reflects whether the base family accepts image input (M8); text-only
// third-party gateway models correctly stay non-vision. Values are approximate
// and deployment-dependent — hosts can override a specific id via modelMetaTable.
var modelMetaFamilies = []struct {
	prefix string
	meta   modelMeta
}{
	// Anthropic Claude (200K context, vision).
	{"claude-opus", modelMeta{contextWindow: 200_000, vision: true}},
	{"claude-sonnet", modelMeta{contextWindow: 200_000, vision: true}},
	{"claude-haiku", modelMeta{contextWindow: 200_000, vision: true}},
	// OpenAI GPT-5 family (400K context, vision).
	{"gpt-5", modelMeta{contextWindow: 400_000, vision: true}},
	{"gpt-4.1", modelMeta{contextWindow: 1_000_000, vision: true}},
	{"gpt-4o", modelMeta{contextWindow: 128_000, vision: true}},
	// OpenAI reasoning models (o-series): large context, vision, reasoning.
	{"o3", modelMeta{contextWindow: 200_000, vision: true, reasoning: true}},
	{"o4", modelMeta{contextWindow: 200_000, vision: true, reasoning: true}},
	// Google Gemini (very large context, vision, reasoning on 2.5+/3.x).
	{"gemini-3", modelMeta{contextWindow: 1_000_000, vision: true, reasoning: true}},
	{"gemini-2.5", modelMeta{contextWindow: 1_000_000, vision: true, reasoning: true}},
	{"gemini-2.0", modelMeta{contextWindow: 1_000_000, vision: true}},
	{"gemma", modelMeta{contextWindow: 128_000}},
	// Common third-party models reachable via the gateway. Text-only bases;
	// context windows are the widely-published values (deployment may differ).
	{"kimi", modelMeta{contextWindow: 128_000}},
	{"glm", modelMeta{contextWindow: 200_000}},
	{"deepseek", modelMeta{contextWindow: 128_000, reasoning: true}},
	{"qwen", modelMeta{contextWindow: 128_000}},
}

// lookupModelMeta resolves metadata for a normalized name: an exact override
// first, then the first matching family prefix. The second return is false when
// neither matches (caller keeps text-only / unknown-window defaults).
func lookupModelMeta(name string) (modelMeta, bool) {
	if m, ok := modelMetaTable[name]; ok {
		return m, true
	}
	for _, f := range modelMetaFamilies {
		if strings.HasPrefix(name, f.prefix) {
			return f.meta, true
		}
	}
	return modelMeta{}, false
}

// normalizeModelName reduces a model id to the bare, dotted name used as a key
// in the pricing and metadata tables (mirrors GetModelPricing's normalization).
func normalizeModelName(id string) string {
	n := strings.ToLower(id)
	n = strings.TrimPrefix(n, "models/")
	n = strings.TrimPrefix(n, "llmgateway/")
	// Strip a remaining provider prefix (anthropic/, openai/, google/).
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return versionDotReplacer.Replace(n)
}

// apiForProvider maps a model id's provider prefix to its wire API. Only
// llmgateway (openai-completions) is a supported target for this port.
func apiForProvider(provider string) string {
	switch provider {
	case "anthropic":
		return APIAnthropicMessages
	case "google":
		return APIGoogle
	default:
		// llmgateway, openai, and the empty prefix all speak OpenAI completions.
		return APIOpenAICompletions
	}
}

// ResolveModel builds a Model catalog row for a model id, merging pricing.json
// rates (M2 honesty preserved via PricingOK) with known capability metadata.
// It never errors: an unpriced or unknown model still yields a usable row with
// PricingOK=false and conservative text-only defaults.
func ResolveModel(id string) Model {
	provider, _, _ := strings.Cut(id, "/")
	if !strings.Contains(id, "/") {
		provider = ""
	}

	m := Model{
		ID:       id,
		Provider: provider,
		API:      apiForProvider(provider),
		Input:    []string{InputText},
	}

	if pricing, err := GetModelPricing(id); err == nil {
		m.Cost = pricing
		m.PricingOK = true
		// A non-zero reasoning rate means the model bills reasoning tokens.
		if pricing.Reasoning > 0 {
			m.Reasoning = true
		}
	}

	if meta, ok := lookupModelMeta(normalizeModelName(id)); ok {
		m.ContextWindow = meta.contextWindow
		if meta.vision {
			m.Input = append(m.Input, InputImage)
		}
		if meta.reasoning {
			m.Reasoning = true
		}
	}

	return m
}

// SupportsImage reports whether the model accepts image input. Used by the
// multimodal path to avoid silently dropping images on a text-only model (M8).
func (m Model) SupportsImage() bool {
	for _, in := range m.Input {
		if in == InputImage {
			return true
		}
	}
	return false
}
