# Pricing: Gateway vs Local Estimate

## Where cost data lives

| Call type | `x-litellm-response-cost` | `x-litellm-response-cost-original` | SSE final chunk `usage` |
|---|---|---|---|
| Non-streaming | present, accurate | present, accurate | n/a |
| Streaming | **absent** | `0.0` (unknown at header time) | tokens only, no cost |

LiteLLM computes cost after token counts are known. For non-streaming, that's before the response headers are sent. For streaming, headers are sent before the body completes, so cost is `0.0` and the `x-litellm-response-cost` header is omitted entirely.

Direct Anthropic and Google APIs do not return pricing information in any form — only token usage. For those providers, local `pricing.json` estimation is the only option.

## What this means for the cost-capture transport (Option A)

A `costTransport` wrapping `http.RoundTripper` can capture `x-litellm-response-cost` for:

- `GenerateObject` calls (structured output, compaction) — non-streaming, header present
- `Generate` calls (if any) — non-streaming, header present

It **cannot** capture cost for:

- `Stream` calls (the main agent loop) — header absent, value `0.0`

For streaming, the kernel must fall back to `CalculateCost` from `pricing.json` using the token counts in `fantasy.Usage` reported by `OnStepFinish`.

## Hybrid strategy

```
OnStepFinish(step):
  u.FromFantasyUsage(step.Usage, model)     // local estimate from pricing.json
  if costCapture != nil:
    costs = costCapture.Drain()             // gateway-reported costs (non-streaming only)
    if len(costs) > 0:
      u.Cost = sum(costs)                   // prefer actual cost when available
  // else: u.Cost stays as local estimate
```

The `costCapture` buffer is nil for non-gateway providers (anthropic, google, openai-direct), so they always use local estimation. For gateway providers, non-streaming calls get actual cost and streaming calls get the local estimate.

## Other gateway headers available

| Header | Value | Notes |
|---|---|---|
| `x-litellm-key-spend` | `213.01` | Cumulative spend for this API key (not per-call) |
| `x-litellm-call-id` | UUID | Per-call ID for correlation |
| `x-litellm-model-api-base` | `https://api.fireworks.ai/inference/v1` | Upstream provider |
| `x-litellm-model-group` | `glm-5p2` | Model group alias |
| `x-litellm-response-duration-ms` | `1135.8` | Upstream latency |
