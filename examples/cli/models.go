package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

// Keep the CLI's compact names while sharing the live gateway catalog reader
// with the kernel prompt.
type modelPricing = toroid.GatewayModelPricing
type listedModel = toroid.GatewayModel

// perMTok scales a micro-USD-per-million-token price to USD per 1M tokens.
func perMTok(v float64) float64 { return toroid.PerMTok(v) }

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

func fetchModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]listedModel, error) {
	return toroid.FetchGatewayModels(ctx, client, baseURL, apiKey)
}
