package toroid

import "strings"

// Model is a catalog row describing one runnable model: how to reach it
// (provider + wire api) and its capabilities (context window, input
// modalities, reasoning).
type Model struct {
	// ID is the full model id as the host uses it, e.g. "llmgateway/kimi-k2p6".
	ID string
	// Provider is the id prefix before the first "/", e.g. "llmgateway".
	Provider string
	// API is the wire protocol used to reach the model.
	API string
	// ContextWindow is the model's max context in tokens, or 0 when unknown.
	ContextWindow int
	// Reasoning reports whether the model emits (and bills) reasoning tokens.
	Reasoning bool
	// PromptCache reports whether the model needs explicit cache_control
	// breakpoints to cache the prompt prefix (opt-in caching). OpenAI-family
	// routes auto-cache and stay false.
	PromptCache bool
	// Input lists supported input modalities, e.g. ["text"] or ["text","image"].
	Input []string
}

// versionDotReplacer normalizes dash-separated version suffixes in model IDs
// to the dotted form used in the metadata tables (e.g. "claude-sonnet-4-6" ->
// "claude-sonnet-4.6"). Intentionally conservative — only literal
// single-digit/single-digit pairs are rewritten, so param-size suffixes like
// "gemma-4-26b" are left untouched.
var versionDotReplacer = strings.NewReplacer(
	"-4-8", "-4.8", "-4-7", "-4.7", "-4-6", "-4.6", "-4-5", "-4.5", "-4-1", "-4.1",
	"-3-5", "-3.5", "-3-1", "-3.1",
	"-2-5", "-2.5", "-2-0", "-2.0",
	"-5-5", "-5.5", "-5-4", "-5.4", "-5-3", "-5.3", "-5-2", "-5.2", "-5-1", "-5.1",
)

// Wire API identifiers. Toroid speaks only OpenAI-compatible chat completions
// (LiteLLM gateway / llmgateway, or OpenAI direct).
const (
	APIOpenAICompletions = "openai-completions"
)

// Input modality identifiers.
const (
	InputText  = "text"
	InputImage = "image"
)

// modelMeta carries the facts about a model that no API exposes: context
// window, modalities, and cache behaviour. Intentionally small and explicit —
// we seed only what we can state truthfully.
type modelMeta struct {
	contextWindow int
	vision        bool
	reasoning     bool
	// promptCache marks a model family that needs explicit cache_control
	// breakpoints to benefit from prompt caching (opt-in caching); OpenAI-family
	// routes auto-cache and stay false.
	promptCache bool
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
// (e.g. all "gpt-5.4-*" get a 400K vision window). Ordered most-specific
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
	// Claude 3 and 4 generation families accept image input via the gateway.
	{"claude-3", modelMeta{vision: true}},
	{"claude-sonnet-4", modelMeta{vision: true}},
	{"claude-opus-4", modelMeta{vision: true}},
	{"claude-haiku-4", modelMeta{vision: true}},
	// OpenAI GPT-5 family (400K context, vision). Most-specific prefix first.
	{"gpt-5.4-nano", modelMeta{contextWindow: 400_000, vision: true}},
	{"gpt-5.4-mini", modelMeta{contextWindow: 400_000, vision: true}},
	{"gpt-5.4-pro", modelMeta{contextWindow: 400_000, vision: true}},
	{"gpt-5.4", modelMeta{contextWindow: 400_000, vision: true}},
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
	{"minimax", modelMeta{contextWindow: 200_000, reasoning: true}},
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
	// Strip a remaining provider prefix (openai/, or a gateway provider id).
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return versionDotReplacer.Replace(n)
}

// ResolveModel builds a Model catalog row for a model id from the capability
// metadata tables. It never errors: an unknown model yields a usable row with
// conservative text-only defaults.
func ResolveModel(id string) Model {
	provider, _, _ := strings.Cut(id, "/")
	if !strings.Contains(id, "/") {
		provider = ""
	}

	m := Model{
		ID:       id,
		Provider: provider,
		API:      APIOpenAICompletions,
		Input:    []string{InputText},
	}

	if meta, ok := lookupModelMeta(normalizeModelName(id)); ok {
		m.ContextWindow = meta.contextWindow
		if meta.vision {
			m.Input = append(m.Input, InputImage)
		}
		if meta.reasoning {
			m.Reasoning = true
		}
		m.PromptCache = meta.promptCache
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
