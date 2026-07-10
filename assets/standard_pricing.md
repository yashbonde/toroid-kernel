# Pricing: standard view, local estimate, and gateway truth

How toroid turns token usage into dollars, what is implemented today, and how
to build a reliable cost view (including gateway models that currently show
`$0.00`).

Source of rates: [`pricing.json`](./pricing.json) · currency FX: [`usd_x.json`](./usd_x.json) · code: [`pricing.go`](../pricing.go), [`kernel.go`](../kernel.go).

---

## 1. How cost is produced (implemented)

Every billed LLM step eventually goes through:

```text
fantasy.Usage  (Input / Output / Reasoning / CacheRead / CacheCreation)
       │
       ▼
Usage.FromFantasyUsage(usage, modelID)     // pricing.go
       │
       ├─ copy token fields
       └─ Cost = CalculateCost(modelID, usage, "USD")
              │
              ├─ GetModelPricing(modelID)   // assets/pricing.json
              └─ USD = Input×Prompt
                     + (Output−Reasoning)×Completion
                     + Reasoning×Reasoning
                     + CacheRead×CacheRead
                     + CacheWrite×CacheWrite
       │
       ▼
recordUsage → runningCostUSD
            → EventTurnCost (TurnUsage, TurnCostUSD, TotalCostUSD)
            → Store.AppendCost (when Save: true)
```

| Surface | What you get |
|---------|----------------|
| `EventTurnCost` | Per-step tokens + estimated USD + running total |
| `Kernel.RunningCostUSD()` | Session cumulative estimate (includes compact + rolled-up subagents) |
| SQLite `costs` table | Persisted turn/cumulative rows when `Save: true` |
| OTEL / Langfuse export | Token fields mapped in `otlp.go` / `store.go` |

**Rates in `pricing.json` are USD per token**, not per 1M tokens. Example shape:

```json
"anthropic/claude-sonnet-4.5": {
  "Prompt": 3e-06,
  "Completion": 1.5e-05,
  "Reasoning": 0.0,
  "CacheRead": 3e-07,
  "CacheWrite": 3.75e-06
}
```

Direct Anthropic / Google / OpenAI APIs do **not** return dollar cost — only
usage. For those providers, the table estimate is the only option today.

---

## 2. Model ID lookup (`GetModelPricing`)

`GetModelPricing` normalizes then probes keys (exact match preferred):

1. Lowercase the id  
2. Strip `models/`  
3. Strip `llmgateway/` (virtual provider; underlying name is what the table keys)  
4. Build name variants: raw id + dash→dot version rewrite (e.g. `claude-sonnet-4-5` → `claude-sonnet-4.5`)  
5. For each variant, try bare name and prefixes `anthropic/`, `openai/`, `google/`  
6. Last resort: fuzzy prefix match against table keys  

So these can all resolve to the same row if the table has `anthropic/claude-sonnet-4.5`:

- `anthropic/claude-sonnet-4.5`
- `claude-sonnet-4.5`
- `llmgateway/claude-sonnet-4-5`

### Silent miss → `$0.00`

If no row matches, `CalculateCost` errors and `FromFantasyUsage` **ignores the
error**, leaving `Usage.Cost = 0`. Tokens are still filled; only dollars go
missing. That is why gateway models like `llmgateway/kimi-k2p6` show
`$0.000000` in the REPL/bench when `kimi` is absent from the table — not
because the loop failed to count usage.

**Standard view rule:** always show tokens; show `$` only when the model
resolves (or when gateway truth is available). Prefer an explicit
“pricing unavailable” label over a misleading zero.

---

## 3. Three layers of a standard pricing view

A complete cost UI / export should treat these as separate sources of truth:

| Layer | Source | Status | Use for |
|-------|--------|--------|---------|
| **A. Local table estimate** | `pricing.json` via `CalculateCost` | **Implemented** | Default $ on every step |
| **B. Token breakdown** | `Usage` / `EventTurnCost` | **Implemented** | Always; works even when A misses |
| **C. Gateway bill** | LiteLLM `x-litellm-response-cost` | **Implemented for non-stream** (Complete / CompleteObject via the Step layer); absent on stream by design | Prefer over A when present |

### Minimal dashboard shape

```text
┌─────────────────────────────────────────────────────────┐
│ Session / Trace                                         │
│  model: llmgateway/kimi-k2p6                            │
│  pricing key: matched? / "unavailable"                  │
│  total $ (estimate): RunningCostUSD                     │
│  steps: N                                               │
├─────────────────────────────────────────────────────────┤
│ Per step (EventTurnCost)                                │
│  in | out | reasoning | cache_read | cache_write | $    │
├─────────────────────────────────────────────────────────┤
│ Sources                                                 │
│  A estimate: pricing.json (or miss)                     │
│  C gateway:  x-litellm-response-cost when captured      │
└─────────────────────────────────────────────────────────┘
```

### Host wiring example

```go
k.On(toroid.EventTurnCost, func(_ context.Context, e toroid.Event) error {
	p := e.Payload.(*toroid.TurnCostPayload)
	// p.TurnUsage.{Input, Output, Reasoning, CacheRead, CacheWrite, Cost}
	// p.TurnCostUSD, p.TotalCostUSD
	return nil
})
fmt.Println(k.RunningCostUSD())
```

---

## 4. Adding models so $ is non-zero

1. Add a row to [`pricing.json`](./pricing.json) (USD **per token**).  
2. Prefer keys the lookup already tries: bare alias (`kimi-k2p6`), and/or
   `openai/…` / `anthropic/…` / `google/…` for the real family.  
3. Gateway ids do not need an `llmgateway/` key — that prefix is stripped.  
4. Sanity-check:

```go
p, err := toroid.GetModelPricing("llmgateway/kimi-k2p6")
// err == nil and rates non-zero
```

5. Re-run a turn; `EventTurnCost` / `RunningCostUSD` should leave zero.

Keep aliases for every name LiteLLM/upstream exposes (`glm-5p2`, deployment
ids, etc.) or the UI will silently report `$0` again.

Optional FX: pass a currency into `CalculateCost` / use `GetCurrencyMultiplier`
with [`usd_x.json`](./usd_x.json). The live agent path currently bills USD only.

---

## 5. Gateway vs local estimate (LiteLLM)

### Where cost data lives on the gateway

| Call type | `x-litellm-response-cost` | `x-litellm-response-cost-original` | SSE final chunk `usage` |
|---|---|---|---|
| Non-streaming | present, accurate | present, accurate | n/a |
| Streaming | **absent** | `0.0` (unknown at header time) | tokens only, no cost |

LiteLLM computes cost after token counts are known. For non-streaming, that is
before response headers are sent. For streaming, headers go out before the body
completes, so cost is `0.0` and `x-litellm-response-cost` is omitted.

### Hybrid strategy (Option A — implemented for non-stream)

The gateway transport (`traceTransport` in `provider.go`) captures
`x-litellm-response-cost` for:

- Non-streaming `Generate` / `GenerateObject` (compact, structured output, …)

It **cannot** capture cost for:

- Streaming `Stream` (main agent loop) — header absent

Implemented via a per-call sink carried in the request context
(`costcapture.go`). A Step attaches the sink before a non-streaming call, the
transport writes any captured cost into it, and the Step prefers that gateway
truth over the local estimate:

```text
FantasyStep.Complete / CompleteObject:
  ctx, sink = withCostSink(ctx)             // only on non-streaming calls
  resp = LM.Generate(ctx, ...)              // transport fills sink from the header
  u.FromFantasyUsage(resp.Usage, model.ID)  // local estimate from pricing.json
  applyGatewayCost(ctx, &u)                 // override with gateway cost when present
```

- Non-gateway providers (`anthropic`, `google`, `openai`): table only (no sink is
  filled, so the estimate stands).
- Gateway: prefer header when present; stream steps stay on the table (no sink
  attached on `Stream`, and the header is absent there anyway).
- A `0.0` header (LiteLLM's streaming placeholder) is ignored so it never
  clobbers a real local estimate.

### Other gateway headers

| Header | Notes |
|---|---|
| `x-litellm-key-spend` | Cumulative spend for this API key (not per-call) |
| `x-litellm-call-id` | Per-call ID for correlation |
| `x-litellm-model-api-base` | Upstream provider base URL |
| `x-litellm-model-group` | Model group alias (e.g. `glm-5p2`) |
| `x-litellm-response-duration-ms` | Upstream latency |
| `x-litellm-session-id` | Set by toroid’s gateway transport for chat grouping |

---

## 6. Cache tokens and prompt-cache wiring (cost-relevant)

`Usage` already tracks `CacheRead` / `CacheWrite` and prices them when the
table has non-zero `CacheRead` / `CacheWrite` rates and the provider reports
those token counts.

**Requesting** cache is separate from **pricing** cache hits:

| Mechanism | Status |
|-----------|--------|
| Report/price `cache_read` / `cache_write` when the API returns them | Implemented |
| Anthropic-style `cache_control` breakpoints via Fantasy `PrepareStep` + last tool | Implemented when `Config.PromptCache` is true (default) |
| Single system prompt (Fantasy `WithSystemPrompt` only — not also in History) | Implemented (stable prefix; avoids double-billing system text) |
| OpenAI-compat / `llmgateway` translating those Anthropic options | **No** — options are ignored on that wire; any `cache_read` is provider/gateway automatic caching |
| Session affinity / LiteLLM cache headers as first-class knobs | Not implemented |

Implication for a standard view: treat `cache_read` / `cache_write` as
**observed usage**, not proof that toroid successfully requested caching for
that provider.

---

## 7. What is counted vs still easy to misread

| Included in `RunningCostUSD` / turn costs (when table matches) | Notes |
|----------------------------------------------------------------|--------|
| Main agent stream steps | `OnStepFinish` → `recordUsage` |
| Compaction summarize call | Uses `SmallerModel` when set; tracked |
| Subagent spend | Rolled into parent after `RunSubagent` |
| Structured `GenerateObject` after tools | Now billed: the schema pass runs through `Step.CompleteObject` and its usage is recorded via `recordUsage` (M7) |

| Common “$0 / wrong $” causes | Fix |
|------------------------------|-----|
| Model missing from `pricing.json` | Add alias (see §4) |
| Gateway stream has no cost header | Expected; use table for stream |
| Cache not requested on gateway | Provider-dependent; see §6 |
| Comparing only `$` without tokens | Always show layer B |

---

## 8. Checklist: shipping a standard pricing view

1. **Coverage** — every production model id (including gateway aliases) has a
   `pricing.json` row; rates in USD per token.  
2. **Miss UX** — “pricing unavailable” + raw tokens, never a quiet `$0.00`
   that looks like free.  
3. **Events** — subscribe to `EventTurnCost`; surface
   `RunningCostUSD` / session totals.  
4. **Persist** — `Save: true` if you need SQLite / OTEL history.  
5. **Gateway hybrid** — when implementing `costTransport`, prefer LiteLLM
   headers on non-stream calls; keep table for stream.  
6. **Cache columns** — show `cache_read` / `cache_write` next to input/output
   so savings (or lack of them) are visible.  
7. **Subagents / compact** — totals should include them (kernel does for
   compact + parent rollup); label child spans if you show a tree.

---

## 9. Related files

| File | Role |
|------|------|
| [`pricing.json`](./pricing.json) | Per-model USD/token rates |
| [`usd_x.json`](./usd_x.json) | Currency multipliers |
| [`pricing.go`](../pricing.go) | Lookup, `CalculateCost`, `Usage` |
| [`kernel.go`](../kernel.go) | `recordUsage`, compact/subagent accounting, prompt-cache opts |
| [`provider.go`](../provider.go) | Gateway transport / session headers |
| [`events.go`](../events.go) | `EventTurnCost`, `TurnCostPayload` |
