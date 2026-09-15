package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

const maxModelsResponseSize = 4 << 20

// modelPricing mirrors the gateway's per-model pricing object (/models
// response). Unit is micro-USD per million tokens. It is the authoritative
// price the gateway bills at.
type modelPricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type modelsResponse struct {
	Data []struct {
		ID            string        `json:"id"`
		ContextLength int           `json:"context_length"`
		Pricing       *modelPricing `json:"pricing"`
	} `json:"data"`
}

// perMTok scales a micro-USD-per-million-token price to USD per 1M tokens.
func perMTok(v float64) float64 { return v / 1e6 }

// runModels implements `trk models`. Model IDs are printed one per line so the
// output remains useful both at a terminal and in shell pipelines. When the
// gateway reports a model's context window it is shown after the id in
// parentheses; models without a reported window print id only. The endpoint
// that was called and any gateway-reported per-token rates are rendered in
// grey.
func runModels(ctx context.Context, out io.Writer, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("takes no arguments")
	}

	baseURL := strings.TrimSpace(os.Getenv(toroid.GatewayBaseURLEnv))
	if baseURL == "" {
		return fmt.Errorf("%s is not set", toroid.GatewayBaseURLEnv)
	}
	apiKey := strings.TrimSpace(os.Getenv(toroid.GatewayKeyEnv))
	if apiKey == "" {
		return fmt.Errorf("%s is not set", toroid.GatewayKeyEnv)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	models, err := fetchModels(ctx, client, baseURL, apiKey)
	if err != nil {
		return err
	}

	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"
	fmt.Fprintf(out, "%sGET %s%s\n", aGray, endpoint, aReset)
	for _, model := range models {
		line := model.ID
		if model.ContextLength > 0 {
			line = fmt.Sprintf("%s (%d)", model.ID, model.ContextLength)
		}
		if rate := priceSuffix(model.Pricing); rate != "" {
			fmt.Fprintf(out, "%s  %s%s%s\n", line, aGray, rate, aReset)
		} else {
			fmt.Fprintln(out, line)
		}
	}
	return nil
}

// priceSuffix renders a model's gateway-reported pricing as a compact grey
// "in / out per MTok" string, or "" when the gateway reported none. The
// gateway's /models pricing (micro-USD per million tokens) is the billing
// source of truth, so it is shown verbatim and there is no local rate table.
func priceSuffix(p *modelPricing) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("$%.2f / $%.2f per MTok", perMTok(p.Input), perMTok(p.Output))
}

type listedModel struct {
	ID            string
	ContextLength int
	Pricing       *modelPricing
}

func fetchModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]listedModel, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "trk")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxModelsResponseSize {
		return nil, fmt.Errorf("response exceeds %d bytes", maxModelsResponseSize)
	}
	if resp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if len(message) > 500 {
			message = message[:500] + "..."
		}
		return nil, fmt.Errorf("gateway returned %s: %s", resp.Status, message)
	}

	var payload modelsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	models := make([]listedModel, 0, len(payload.Data))
	for _, model := range payload.Data {
		if id := strings.TrimSpace(model.ID); id != "" {
			models = append(models, listedModel{ID: id, ContextLength: model.ContextLength, Pricing: model.Pricing})
		}
	}
	return models, nil
}
