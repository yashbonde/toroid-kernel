package toroid

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Model limits discovery ------------------------------------------------------
//
// The static catalog in model.go only knows the model families compiled into
// it, so a gateway-hosted id it has never heard of resolves with
// ContextWindow == 0 and no output ceiling. The kernel then sends whatever the
// caller configured (or nothing at all) and the provider silently truncates
// mid-answer with finish_reason=length, which the step loop reports as a clean
// stop.
//
// OpenAI-compatible gateways expose per-model metadata at GET /v1/models/{id},
// so ask the gateway once at kernel startup and clamp the context/output knobs
// to what the model actually supports. The probe is strictly best-effort: any
// error, timeout, or unparseable body leaves the configured values untouched.

// modelLimits is the subset of a gateway's /v1/models/{id} document the kernel
// acts on. Gateways vary in spelling, so accept both the max_output_tokens and
// max_tokens forms for the output ceiling.
type modelLimits struct {
	MaxInputTokens  int `json:"max_input_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
	MaxTokens       int `json:"max_tokens"`
	// ContextLength is OpenRouter's spelling for the model's total context
	// window. Used as the input/context ceiling when the gateway does not
	// report max_input_tokens.
	ContextLength int `json:"context_length"`
}

// inputContext returns the model's total context window: max_input_tokens when
// present, otherwise OpenRouter's context_length.
func (m modelLimits) inputContext() int {
	if m.MaxInputTokens > 0 {
		return m.MaxInputTokens
	}
	return m.ContextLength
}

// outputCeiling returns the model's max output tokens under either spelling.
func (m modelLimits) outputCeiling() int {
	if m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	return m.MaxTokens
}

// modelLimitsTimeout bounds the startup probe. A gateway that cannot answer in
// this long is not worth delaying every run for — the configured defaults are
// a fine fallback.
const modelLimitsTimeout = 5 * time.Second

// fetchModelLimits asks an OpenAI-compatible gateway for one model's metadata.
// ok is false whenever the limits could not be established, in which case the
// caller must keep its existing configuration.
func fetchModelLimits(ctx context.Context, baseURL, apiKey, model string) (modelLimits, bool) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	// The wire client strips this prefix before talking to the gateway, so the
	// metadata lookup has to use the same bare id (see llm/client.go).
	id := strings.TrimPrefix(model, "llmgateway/")
	if base == "" || id == "" {
		return modelLimits{}, false
	}

	ctx, cancel := context.WithTimeout(ctx, modelLimitsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models/"+id, nil)
	if err != nil {
		return modelLimits{}, false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return modelLimits{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return modelLimits{}, false // no such route, unknown model, bad key
	}

	var lim modelLimits
	if err := json.NewDecoder(resp.Body).Decode(&lim); err != nil {
		return modelLimits{}, false
	}
	if lim.inputContext() <= 0 && lim.outputCeiling() <= 0 {
		return modelLimits{}, false // answered, but told us nothing usable
	}
	return lim, true
}

// applyModelLimits clamps the context and output knobs to what the model can
// actually serve. Configured values are honoured when they fit and lowered when
// they do not; an unset knob adopts the model's own ceiling. The context window
// comes from max_input_tokens or, failing that, OpenRouter's context_length.
// Limits the gateway did not report are left alone. Pure, so the clamping rules
// are testable without a gateway.
func applyModelLimits(cfg *Config, lim modelLimits) {
	if in := lim.inputContext(); in > 0 {
		if cfg.TotalContextSize <= 0 || cfg.TotalContextSize > in {
			cfg.TotalContextSize = in
		}
		// The compaction buffer is carved out of the context window: at or above
		// it, the threshold collapses to zero (or negative) and every turn
		// compacts. Keep a quarter of the window as the sensible fallback.
		if cfg.CompactionBufferSize >= cfg.TotalContextSize {
			cfg.CompactionBufferSize = cfg.TotalContextSize / 4
		}
	}
	if out := lim.outputCeiling(); out > 0 {
		if cfg.MaxTokens <= 0 || cfg.MaxTokens > out {
			cfg.MaxTokens = out
		}
	}
}
