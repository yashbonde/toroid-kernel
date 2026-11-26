package toroid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	maxGatewayModelsResponseSize = 4 << 20
	gatewayModelsTimeout         = 5 * time.Second
)

// GatewayModelPricing is the gateway-reported price in micro-USD per million
// tokens. It is the same pricing the gateway bills against.
type GatewayModelPricing struct {
	Unit       string  `json:"unit"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// EstimateUSD prices observed token usage at catalog rates, rounded per request.
func (p *GatewayModelPricing) EstimateUSD(u Usage) (float64, bool) {
	if p == nil || p.Unit != "micro_usd_per_million_tokens" || p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite < 0 ||
		u.Input+u.Output+u.CacheRead+u.CacheWrite == 0 {
		return 0, false
	}
	micro := math.Ceil((float64(u.Input)*p.Input + float64(u.Output)*p.Output + float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6)
	return micro / 1e6, true
}

// GatewayModel is one entry returned by an OpenAI-compatible gateway's
// /models endpoint.
type GatewayModel struct {
	ID            string
	ContextLength int
	Pricing       *GatewayModelPricing
}

// FindGatewayModel matches a routed kernel model to its catalog entry.
func FindGatewayModel(models []GatewayModel, id string) GatewayModel {
	id = strings.TrimPrefix(id, "llmgateway/")
	for _, model := range models {
		if model.ID == id {
			return model
		}
	}
	return GatewayModel{}
}

// PerMTok converts the gateway's micro-USD-per-million-token price to USD per
// million tokens for display.
func PerMTok(v float64) float64 { return v / 1e6 }

// FetchGatewayModels reads the live gateway catalog. It is intentionally
// bounded so capability discovery never makes kernel startup depend on a
// responsive catalog endpoint.
func FetchGatewayModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]GatewayModel, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("gateway base URL is empty")
	}
	if client == nil {
		client = http.DefaultClient
	}

	ctx, cancel := context.WithTimeout(ctx, gatewayModelsTimeout)
	defer cancel()
	endpoint := base + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "trk")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayModelsResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxGatewayModelsResponseSize {
		return nil, fmt.Errorf("response exceeds %d bytes", maxGatewayModelsResponseSize)
	}
	if resp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if len(message) > 500 {
			message = message[:500] + "..."
		}
		return nil, fmt.Errorf("gateway returned %s: %s", resp.Status, message)
	}

	var payload struct {
		Data []struct {
			ID             string               `json:"id"`
			ContextLength  int                  `json:"context_length"`
			MaxInputTokens int                  `json:"max_input_tokens"`
			Pricing        *GatewayModelPricing `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	models := make([]GatewayModel, 0, len(payload.Data))
	for _, model := range payload.Data {
		if id := strings.TrimSpace(model.ID); id != "" {
			if model.MaxInputTokens > 0 {
				model.ContextLength = model.MaxInputTokens
			}
			models = append(models, GatewayModel{
				ID:            id,
				ContextLength: model.ContextLength,
				Pricing:       model.Pricing,
			})
		}
	}
	return models, nil
}

const maxGatewayModelsInPrompt = 10

// gatewayModelsPrompt renders a bounded, deterministic live catalog. The
// gateway is the source of truth for which models can actually be selected and
// what they cost; keeping this list short avoids turning a large catalog into
// prompt bloat.
func gatewayModelsPrompt(catalogs ...[]GatewayModel) string {
	if len(catalogs) == 0 {
		return ""
	}
	models := catalogs[0]
	priced := make([]GatewayModel, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) != "" && model.Pricing != nil {
			priced = append(priced, model)
		}
	}
	if len(priced) == 0 {
		return ""
	}

	sort.Slice(priced, func(i, j int) bool {
		left, right := priced[i], priced[j]
		if left.Pricing.Input != right.Pricing.Input {
			return left.Pricing.Input < right.Pricing.Input
		}
		if left.Pricing.Output != right.Pricing.Output {
			return left.Pricing.Output < right.Pricing.Output
		}
		return left.ID < right.ID
	})

	var b strings.Builder
	b.WriteString("\nAvailable gateway models (live catalog; cheapest first; per-MTok in / out):\n")
	shown := min(len(priced), maxGatewayModelsInPrompt)
	for _, model := range priced[:shown] {
		fmt.Fprintf(&b, "%s  $%.2f / $%.2f\n", model.ID, PerMTok(model.Pricing.Input), PerMTok(model.Pricing.Output))
	}
	if remaining := len(priced) - shown; remaining > 0 {
		fmt.Fprintf(&b, "… and %d more; run `trk models` for the full catalog.\n", remaining)
	}
	return b.String()
}
