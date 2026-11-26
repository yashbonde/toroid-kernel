package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	toroid "github.com/yashbonde/toroid-kernel"
)

type chatUsage struct {
	SpendUSD       *float64 `json:"spendUSD"`
	EstimatedSpend bool     `json:"estimatedSpend,omitempty"`
	Used           *int     `json:"used"`
	Window         int      `json:"window"`
	Model          string   `json:"model,omitempty"`
}

// Count each model call on its own span. Ledger rollups include child charges
// twice, and older streaming ledger entries contain the initial reservation.
func (a *app) spend(ctx context.Context, id string) (chatUsage, error) {
	var u chatUsage
	rows, err := a.db.QueryContext(ctx, `SELECT span_id, kind,
  COALESCE(json_extract(data,'$.payload.model'),''),
  json_extract(data,'$.payload.turn_usage')
  FROM events WHERE trace_id=? AND kind IN ('TurnStarted','TurnCompleted','PostCompact','TurnCost')
  AND (kind!='TurnCompleted' OR json_extract(data,'$.payload.stop_reason') IS NOT NULL)
  AND ts>=COALESCE((SELECT started_at FROM traces WHERE trace_id=? AND previous_trace_id!=''),0)
  ORDER BY span_id,seq`, id, id)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	models := make(map[string]string)
	legacy := make(map[string][]toroid.Usage)
	total := 0.0
	for rows.Next() {
		var span, kind, model string
		var raw sql.NullString
		if err := rows.Scan(&span, &kind, &model, &raw); err != nil {
			return u, err
		}
		if kind == "TurnStarted" {
			models[span] = model
			continue
		}
		if !raw.Valid {
			return u, nil
		}
		var step toroid.Usage
		if err := json.Unmarshal([]byte(raw.String), &step); err != nil {
			return u, err
		}
		switch {
		case step.PricingOK && (step.CostSource == "gateway" || step.CostSource == "catalog"):
			total += step.Cost
			u.EstimatedSpend = u.EstimatedSpend || step.CostSource == "catalog"
		case step.PricingOK && step.CostSource == "":
			if model == "" {
				model = models[span]
			}
			if !strings.HasPrefix(model, "llmgateway/") {
				return u, nil
			}
			legacy[model] = append(legacy[model], step)
		default:
			return u, nil
		}
	}
	if err := rows.Err(); err != nil {
		return u, err
	}
	if len(legacy) > 0 {
		base, key, prefix := a.catalogProvider()
		if prefix != "llmgateway/" {
			return u, nil
		}
		catalog := toroid.CachedGatewayModels(ctx, base, key)
		for model, steps := range legacy {
			rate := toroid.FindGatewayModel(catalog, model).Pricing
			for _, step := range steps {
				cost, ok := rate.EstimateUSD(step)
				if !ok {
					return u, nil
				}
				total += cost
			}
		}
		u.EstimatedSpend = true
	}
	u.SpendUSD = &total
	return u, nil
}

func (a *app) savedUsage(ctx context.Context, id string) (chatUsage, error) {
	u, err := a.spend(ctx, id)
	if err != nil {
		return u, err
	}

	err = a.db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(data,'$.payload.model'),''),
		COALESCE(json_extract(data,'$.payload.context_window'),0) FROM events
		WHERE trace_id=? AND span_id=? AND kind='TurnStarted' ORDER BY seq DESC LIMIT 1`, id, id).Scan(&u.Model, &u.Window)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return u, err
	}
	var raw sql.NullString
	var after sql.NullInt64
	err = a.db.QueryRowContext(ctx, `SELECT json_extract(data,'$.payload.turn_usage'),
		json_extract(data,'$.payload.tokens_after') FROM events WHERE trace_id=? AND span_id=?
		AND kind IN ('TurnCompleted','PostCompact') AND (kind='PostCompact' OR json_extract(data,'$.payload.stop_reason') IS NOT NULL)
		ORDER BY seq DESC LIMIT 1`, id, id).Scan(&raw, &after)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return u, err
	}
	if after.Valid {
		used := int(after.Int64)
		u.Used = &used
	} else if raw.Valid {
		var tokens toroid.Usage
		if err := json.Unmarshal([]byte(raw.String), &tokens); err != nil {
			return u, err
		}
		used := int(tokens.Input + tokens.Output + tokens.CacheRead + tokens.CacheWrite)
		if used > 0 {
			u.Used = &used
		}
	}
	return u, nil
}
