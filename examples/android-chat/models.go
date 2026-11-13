package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// chatModel is one entry in the picker. ID is the kernel model id; prices are
// USD per million tokens, the unit OpenRouter's catalog page shows.
type chatModel struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Tier       string  `json:"tier"`
	InPerMTok  float64 `json:"in_per_mtok"`
	OutPerMTok float64 `json:"out_per_mtok"`
	// CachePerMTok prices cached prompt tokens; 0 means "bill at InPerMTok".
	CachePerMTok float64 `json:"cache_per_mtok,omitempty"`
	// Vision reports whether the model accepts image input, taken from
	// OpenRouter's architecture.input_modalities at refresh. It decides whether
	// the apps tools attach a screenshot (and whether the kernel keeps it).
	Vision bool `json:"vision,omitempty"`
}

// gatewayPrefix routes an id through LLM_GATEWAY_BASE_URL; the kernel strips
// it before the request, leaving the bare OpenRouter id.
const gatewayPrefix = "llmgateway/"

// Model classes, cheapest first. The picker asks for a class, then a model in it.
const (
	tierFlash    = "flash"    // fractions of a cent per reply
	tierMid      = "mid"      // stronger, still cheap
	tierFrontier = "frontier" // each lab's flagship
)

// tierInfo is one class as the picker shows it.
type tierInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var tiers = []tierInfo{{tierFlash, "Flash"}, {tierMid, "Mid"}, {tierFrontier, "Frontier"}}

// defaultModels is the curated picker, picked from OpenRouter's public catalog
// (Nov 2026): only the newest release of each model family, so a superseded
// version (DeepSeek V4 Flash next to V4.1 Flash) never shows. Flash is mostly
// open-weight models from Chinese labs; Mid and Frontier add the big labs. The
// prices here are a fallback; catalog.refresh replaces them with the live ones
// at startup. The first Flash model is the default.
var defaultModels = []chatModel{
	{ID: gatewayPrefix + "deepseek/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Tier: tierFlash, InPerMTok: 0.022, OutPerMTok: 0.40},
	{ID: gatewayPrefix + "z-ai/glm-5.3-flash", Name: "GLM 5.3 Flash", Tier: tierFlash, InPerMTok: 0.04, OutPerMTok: 0.078},
	{ID: gatewayPrefix + "qwen/qwen3.8-flash", Name: "Qwen3.8 Flash", Tier: tierFlash, InPerMTok: 0.15, OutPerMTok: 0.47},
	{ID: gatewayPrefix + "xiaomi/mimo-v2.6-flash", Name: "MiMo V2.6 Flash", Tier: tierFlash, InPerMTok: 0.14, OutPerMTok: 0.28},
	{ID: gatewayPrefix + "minimax/minimax-m3.1-flash", Name: "MiniMax M3.1 Flash", Tier: tierFlash, InPerMTok: 0.15, OutPerMTok: 0.60},
	{ID: gatewayPrefix + "anthropic/claude-haiku-5.5", Name: "Claude Haiku 5.5", Tier: tierFlash, InPerMTok: 0.10, OutPerMTok: 0.50},
	{ID: gatewayPrefix + "openai/gpt-6-luna", Name: "GPT-6 Luna", Tier: tierFlash, InPerMTok: 0.10, OutPerMTok: 0.50},

	{ID: gatewayPrefix + "z-ai/glm-5.3", Name: "GLM 5.3", Tier: tierMid, InPerMTok: 0.07, OutPerMTok: 1.40},
	{ID: gatewayPrefix + "qwen/qwen3.7-plus", Name: "Qwen3.7 Plus", Tier: tierMid, InPerMTok: 0.32, OutPerMTok: 1.28},
	{ID: gatewayPrefix + "xiaomi/mimo-v2.6-pro", Name: "MiMo V2.6 Pro", Tier: tierMid, InPerMTok: 0.435, OutPerMTok: 0.87},
	{ID: gatewayPrefix + "minimax/minimax-m3", Name: "MiniMax M3", Tier: tierMid, InPerMTok: 0.30, OutPerMTok: 1.20},
	{ID: gatewayPrefix + "tencent/hy4", Name: "Tencent Hy4", Tier: tierMid, InPerMTok: 0.417, OutPerMTok: 1.25},
	{ID: gatewayPrefix + "deepseek/deepseek-v4-pro-0813", Name: "DeepSeek V4 Pro", Tier: tierMid, InPerMTok: 0.111, OutPerMTok: 4.44},
	{ID: gatewayPrefix + "mistralai/mistral-large-4-0", Name: "Mistral Large 4", Tier: tierMid, InPerMTok: 0.70, OutPerMTok: 2.10},
	{ID: gatewayPrefix + "google/gemini-3.8-flash", Name: "Gemini 3.8 Flash", Tier: tierMid, InPerMTok: 0.825, OutPerMTok: 4.125},

	{ID: gatewayPrefix + "moonshotai/kimi-k3.1-pro", Name: "Kimi K3.1 Pro", Tier: tierFrontier, InPerMTok: 0.237, OutPerMTok: 8.409},
	{ID: gatewayPrefix + "qwen/qwen3.8-max-0902", Name: "Qwen3.8 Max", Tier: tierFrontier, InPerMTok: 2.0, OutPerMTok: 6.0},
	{ID: gatewayPrefix + "z-ai/glm-5.3-prime", Name: "GLM 5.3 Prime", Tier: tierFrontier, InPerMTok: 2.8, OutPerMTok: 8.8},
	{ID: gatewayPrefix + "x-ai/grok-4.7", Name: "Grok 4.7", Tier: tierFrontier, InPerMTok: 2.0, OutPerMTok: 6.0},
	{ID: gatewayPrefix + "google/gemini-4-carbon", Name: "Gemini 4 Carbon", Tier: tierFrontier, InPerMTok: 2.0, OutPerMTok: 10.0},
	{ID: gatewayPrefix + "openai/gpt-6.1-sol", Name: "GPT-6.1 Sol", Tier: tierFrontier, InPerMTok: 2.0, OutPerMTok: 10.0},
	{ID: gatewayPrefix + "anthropic/claude-sonnet-5.5", Name: "Claude Sonnet 5.5", Tier: tierFrontier, InPerMTok: 2.0, OutPerMTok: 10.0},
	{ID: gatewayPrefix + "anthropic/claude-opus-5.5", Name: "Claude Opus 5.5", Tier: tierFrontier, InPerMTok: 4.0, OutPerMTok: 20.0},
	{ID: gatewayPrefix + "anthropic/claude-fable-5.2", Name: "Claude Fable 5.2", Tier: tierFrontier, InPerMTok: 8.0, OutPerMTok: 40.0},
}

// catalog is the live model list. It starts as defaultModels and is refreshed
// once from the gateway's /models endpoint.
type catalog struct {
	mu     sync.RWMutex
	models []chatModel
	// liveVision is every OpenRouter id (bare, without gatewayPrefix) whose
	// input_modalities include images, from the last refresh; nil until one
	// succeeds. It vets the vision-delegate setting, which may name a model
	// outside the curated picker.
	liveVision map[string]bool
}

// visionFallbackFamilies marks a built-in model as image-capable when the live
// OpenRouter catalog (the authoritative source via architecture.input_modalities)
// cannot be reached. It is a best-effort list of families known to take image
// input; refresh overwrites every model's Vision with the real value.
var visionFallbackFamilies = []string{
	"claude", "gpt-6", "gpt-5", "gemini", "grok", "qwen3.8", "mimo", "minimax",
}

func newCatalog() *catalog {
	models := append([]chatModel(nil), defaultModels...)
	for i := range models {
		id := strings.TrimPrefix(models[i].ID, gatewayPrefix)
		for _, fam := range visionFallbackFamilies {
			if strings.Contains(id, fam) {
				models[i].Vision = true
				break
			}
		}
	}
	return &catalog{models: models}
}

func (c *catalog) list() []chatModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]chatModel(nil), c.models...)
}

// visionFor reports whether model id accepts image input (OpenRouter's
// architecture.input_modalities at refresh, else the built-in fallback). An
// unknown id is treated as text-only.
func (c *catalog) visionFor(id string) bool {
	m, ok := c.lookup(id)
	return ok && m.Vision
}

// acceptsImages reports whether id (with gatewayPrefix) takes image input:
// known reports whether the live catalog has been loaded to say so.
func (c *catalog) acceptsImages(id string) (vision, known bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.liveVision == nil {
		return false, false
	}
	return c.liveVision[strings.TrimPrefix(id, gatewayPrefix)], true
}

func (c *catalog) lookup(id string) (chatModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.models {
		if m.ID == id {
			return m, true
		}
	}
	return chatModel{}, false
}

// orModels is the slice of OpenRouter's /models response this app reads.
// Prices are decimal strings in USD per token.
type orModels struct {
	Data []struct {
		ID           string `json:"id"`
		Architecture struct {
			InputModalities []string `json:"input_modalities"`
		} `json:"architecture"`
		Pricing struct {
			Prompt         string `json:"prompt"`
			Completion     string `json:"completion"`
			InputCacheRead string `json:"input_cache_read"`
		} `json:"pricing"`
	} `json:"data"`
}

// refresh fetches baseURL/models and updates each curated model's prices.
// Curated models missing from a non-empty catalog are dropped, so the picker
// never offers an id the gateway would reject. On any error the list is left
// untouched.
func (c *catalog) refresh(ctx context.Context, hc *http.Client, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /models: %s", resp.Status)
	}
	var body orModels
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode /models: %w", err)
	}
	if len(body.Data) == 0 {
		return fmt.Errorf("GET /models: empty catalog")
	}

	live := make(map[string]chatModel, len(body.Data))
	liveVision := map[string]bool{}
	for _, d := range body.Data {
		for _, mod := range d.Architecture.InputModalities {
			if mod == "image" {
				liveVision[d.ID] = true
			}
		}
		in, errIn := perMTok(d.Pricing.Prompt)
		out, errOut := perMTok(d.Pricing.Completion)
		if errIn != nil || errOut != nil {
			continue
		}
		m := chatModel{InPerMTok: in, OutPerMTok: out}
		if cr, err := perMTok(d.Pricing.InputCacheRead); err == nil {
			m.CachePerMTok = cr
		}
		for _, mod := range d.Architecture.InputModalities {
			if mod == "image" {
				m.Vision = true
			}
		}
		live[d.ID] = m
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.liveVision = liveVision
	kept := c.models[:0:0]
	for _, m := range c.models {
		p, ok := live[strings.TrimPrefix(m.ID, gatewayPrefix)]
		if !ok {
			continue
		}
		m.InPerMTok, m.OutPerMTok, m.CachePerMTok, m.Vision = p.InPerMTok, p.OutPerMTok, p.CachePerMTok, p.Vision
		kept = append(kept, m)
	}
	if len(kept) == 0 {
		return fmt.Errorf("none of the curated models are in the live catalog")
	}
	c.models = kept
	return nil
}

// perMTok converts a per-token USD string to USD per million tokens, rounded
// to a micro-dollar so float noise does not show up in the picker.
func perMTok(perToken string) (float64, error) {
	v, err := strconv.ParseFloat(perToken, 64)
	if err != nil {
		return 0, err
	}
	return math.Round(v*1e12) / 1e6, nil
}
