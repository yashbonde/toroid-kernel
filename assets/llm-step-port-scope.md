# LLM step layer — selective port scope (handoff)

**Audience:** another model or engineer implementing work.  
**Goal:** adopt **only** the pi-relevant features we need for toroid — not a fork of pi, not a Fantasy clone of every provider.  
**Status:** scope freeze; **library + seams landed** on branch `worktree-llm-step-port`; **kernel integration still partial** (see §14–§15).

> **Implementation status (branch `worktree-llm-step-port`, reviewed 2026-07-10).**  
> Structure is **correct and supports** this document. Fantasy retained as the first backend.  
> **Done at the library / schema-path level:**
>
> - **Phase A** — `Usage.PricingOK` honesty flag (no silent `$0`), cached pricing
>   table, `Model` catalog (`model.go`, `ResolveModel`), multimodal 5 MiB cap +
>   vision-capability gate with surfaced warnings (`multimodal.go`).
> - **Phase B (API + adapter)** — `Step` / `Context` / `AssistantMessage` / `ObjectResult`
>   (`step.go`); `FantasyStep` adapter — one Fantasy call per llm-step
>   (`fantasystep.go`); the `WithSchema` object pass runs through
>   `Step.CompleteObject` **and is billed** (M7 fixed).  
>   **Not done:** preferred kernel-owned tool loop (`streamCurrent` still uses Fantasy `Agent.Stream`).
> - **Phase C (lite)** — no native openai client; Fantasy `openaicompat` covers
>   `llmgateway/*`. Captures `x-litellm-response-cost` on non-stream calls
>   (`costcapture.go`, `provider.go`). Stream path still uses local estimate.
> - **Phase D (library)** — `FauxStep`, `OnPayload`, abort-keeps-partial on Step
>   stream, `TransformForHandoff` (implemented + unit-tested; **not wired** into
>   Compact / subagent).
>
> Tests (network-free, all pass): `pricing_test.go`, `model_test.go`,
> `multimodal_test.go`, `step_test.go`, `costcapture_test.go`, `handoff_test.go`.
>
> **Short verdict:** seams are right; production chat loop is not yet fully on Step.  
> Details: [§14 Review](#14-structure-review-2026-07-10) · [§15 Pending](#15-pending-work).

**Related:**

| Doc | Role |
|-----|------|
| [terminology.md](./terminology.md) | transcript → chat → turn → llm-step |
| [standard_pricing.md](./standard_pricing.md) | cost layers, `pricing.json`, gateway headers |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | current kernel shape |
| External reference (read selectively) | [pi-ai README](https://github.com/earendil-works/pi/blob/main/packages/ai/README.md) — **design inspiration only** |

**Do not:**

- Vendor or transpile `@earendil-works/pi-ai` into this repo.
- Register dozens of providers (Bedrock, Copilot OAuth, MiniMax China, …).
- Move the **agent tool loop** out of the kernel into a new “agent library.”
- Port image **generation**, browser bundles, or OAuth login CLIs.

**Do:**

- Copy **features and contracts** below into Go, in-repo.
- Keep toroid as an **embeddable Go kernel**; host owns UI.
- Prefer a thin **step** API (one LLM call = one **llm-step**) under the existing chat loop.

---

## 1. Product context (toroid)

toroid-kernel is a Go library that runs tool-using coding agents:

- Kernel owns: history, compact, prune, tool registry, subagents, events, SQLite, OTEL, skills, MCP.
- Today the multi-step tool loop is largely delegated to **Fantasy** `Agent.Stream`.
- First-class requirements already in product:
  - **Structured generation** (`WithSchema` / `GenerateObject` after tools).
  - **Multimodal input** (images/PDF in user messages via `multimodal.go`).
  - **Cost visibility** (local `pricing.json`; gateway truth designed, incomplete).
  - **Provider (this port only):** OpenAI-compatible **LiteLLM** via `llmgateway` — skip native Anthropic / Google / OpenAI for now.

### Terminology (must use in code/docs going forward)

See [terminology.md](./terminology.md). Summary:

```text
transcript ── full conversation (one Langfuse session)
 └─ chat ── one human prompt → agent turns → final AI response
    └─ turn ── one tool-loop step (tool call(s) + result(s), or final step)
       └─ llm-step ── one LLM call (one litellm_request)
```

| Term | Meaning |
|------|---------|
| transcript | Full conversation; session-level totals |
| chat | One `Run`/`Stream` answer cycle for a user prompt |
| turn | One agent loop step (`OnStepFinish`); tools run outside the LLM |
| llm-step | Exactly one provider/gateway LLM request |

**Naming:** product **llm-step** = one LLM request. Do not call this “trace” (reserved for OTEL / SQLite `TraceID` / conversation graph).

---

## 2. Architectural split (what we copy from pi’s *shape*)

pi-ai is a **step library** (stream/complete one assistant message; host runs tools).  
Fantasy is a **multi-step agent** (tools + retries inside the library).  
toroid should converge on:

```text
Host
  └─ Kernel (transcript / chat / turns: tools, compact, prune, subagents, events)
       └─ LLM Step (one llm-step: map Context → wire → AssistantMessage + Usage + $)
            └─ Protocol: openai-completions only (LiteLLM / llmgateway)
```

| Layer | Owns | Does not own |
|-------|------|----------------|
| Kernel | Tool loop, turns, compact, subagents, events, persistence | Raw HTTP to every SaaS |
| LLM Step | One **llm-step**: auth, request body, stream events, usage/cost, schema/object call | Multi-turn tool orchestration |
| Protocol | OpenAI-compatible wire + gateway compat flags | Product policy (MaxIter, prune); other native SDKs |

**Near term:** implement Step **behind** or **beside** Fantasy (adapter) for **`llmgateway` only**.  
**Out of scope for this handoff:** native Anthropic, Google, or OpenAI-direct providers (existing code paths may remain; do not invest in them).

---

## 3. Feature matrix — copy / keep / skip

### 3.1 Must copy (implement in toroid)

| # | Feature | Spec for implementer | Acceptance |
|---|---------|----------------------|------------|
| M1 | **Model as data** | Each runnable model has: id, provider, wire `api`, `contextWindow`, `cost` (per-token input/output/cacheRead/cacheWrite), `reasoning` bool, `input` modalities (`text`, `image`, …) | `llmgateway/kimi-k2p6` can resolve cost without silent $0 if catalog has rates |
| M2 | **Usage + cost on every llm-step** | Every LLM response carries tokens + `Cost` USD or explicit `PricingUnavailable` | No quiet `$0.00` when model unknown; UI/events can distinguish free vs unknown |
| M3 | **Caller-owned Context** | `systemPrompt` + `messages` + `tools` as the only input to an llm-step; system not duplicated in message list | Single system blob per request; stable cache prefix |
| M4 | **Step-only API** | `Stream` / `Complete` = one assistant message / one **llm-step**; kernel loops for tools | Kernel can compact/prune/budget **between** llm-steps |
| M5 | **Single wire: openai-completions (LiteLLM)** | Only `llmgateway/*` via OpenAI Chat Completions. Compat as needed: usage-in-stream, max_tokens field name, session affinity / LiteLLM headers. **Do not** add anthropic-messages or google adapters in this phase | `Config.Model = "llmgateway/<name>"` works end-to-end |
| M6 | **Prompt cache (gateway-honest)** | Only what LiteLLM/upstream reports (`cache_read` / `cache_write` in usage). Optional session affinity headers if gateway documents them. Do not implement Anthropic `cache_control` for this phase | Usage fields populated when gateway returns them; no false claims of Anthropic-style breakpoints |
| M7 | **Structured generation first-class** | Keep `WithSchema` as kernel `Run`/`Stream` option. Final object pass is an **llm-step** with full usage/cost. Prefer validated object in result path, not only writer bytes | Schema chat bills and events include the object **llm-step** |
| M8 | **Multimodal input (scoped)** | User (and optionally toolResult) image/PDF; capability check from model metadata; **hard size caps** | Oversized media rejected or truncated with clear error; model without vision does not silent-drop without signal |
| M9 | **Cross-model handoff rules** | When chat switches model (SmallerModel, compact, subagent): preserve tools/results; transform thinking blocks if APIs differ | Subagent/compact on different model does not corrupt history |
| M10 | **Request debug** | `onPayload` or equivalent to log/inspect outbound body for an **llm-step** | Can verify cache_control / tools / schema on the wire |
| M11 | **Abort + partial assistant** | Cancel mid-**llm-step**; keep partial content; allow continue | REPL ESC / context cancel leaves recoverable state |
| M12 | **Faux / scripted model** | In-memory model for tests: scripted assistant messages, optional fake cache usage | Loop/cost tests without network |

### 3.2 Already in toroid — keep; only harden

| Feature | Location | Work if any |
|---------|----------|-------------|
| Tool loop / MaxIter / loop guard | `kernel.go` | Use **turn** terminology in comments/events over time |
| Compact + prune + tool output caps | `kernel.go`, `tools/*` | Pre-trim before compact (done); keep caps |
| Subagents sync/async | `kernel.go` | Use SmallerModel; cost rollup (done) |
| Events + SQLite + OTEL | `events.go`, `store.go`, `otlp.go` | Map metrics to transcript/chat/turn/llm-step |
| Skills + MCP | `skills.go`, `tools/mcp.go` | MCP results already truncated |
| `WithSchema` | `kernel.go` | **Must** track cost on object **llm-step** (M7) |
| Multimodal parse | `multimodal.go` | Add caps + capability (M8) |
| Fantasy `llmgateway` path | `provider.go` | Only path in scope; keep/adapt until Step owns openai-completions |

### 3.3 Explicitly do **not** copy / implement (this phase)

| Skip | Why |
|------|-----|
| Native Anthropic / Google / OpenAI providers | **Out of scope for now** — only LiteLLM `llmgateway` |
| Full provider zoo (Bedrock, Copilot OAuth, …) | Gateway covers long tail upstream |
| OAuth (Claude Pro, Codex, Copilot) | API key via env |
| Image **generation** APIs | Not a coding-kernel feature |
| Browser / tree-shaking / npm packaging | Go module |
| Dynamic refresh of all SaaS model lists | Static catalog + gateway model names |
| TypeBox / TS stack | Go schemas (existing Fantasy schema or std) |
| Moving tool execution into the LLM package | Kernel owns tools |
| Literal port of pi source tree | Reimplement specs only |

---

## 4. Provider / API scope (hard cap for this handoff)

**Only supported target:**

| Priority | Wire API | Model id form | Used for |
|----------|----------|---------------|----------|
| **P0 (only)** | OpenAI-compatible Chat Completions | `llmgateway/<model>` | Self-hosted / company **LiteLLM** gateway |

**Skip for now (do not build or expand):**

- Native `anthropic/*`
- Native `google/*` / Gemini
- Direct `openai/*` (non-gateway)
- Bedrock, Azure, Vertex, OpenRouter-as-first-class, Copilot, etc.

Upstream models behind LiteLLM (Claude, Kimi, Gemini, …) are reached **only** as `llmgateway/<name>` — the kernel speaks OpenAI-compat to LiteLLM; LiteLLM routes.

### Model ids

- Form: `llmgateway/<upstream-name>` (example: `llmgateway/kimi-k2p6`).
- Catalog / `pricing.json` should list the **upstream name** (and aliases) with rates; `llmgateway/` is stripped in lookup (see [standard_pricing.md](./standard_pricing.md)).
- Stream path: local estimate from catalog.
- Non-stream: optional later capture of `x-litellm-response-cost`.

### Env vars for testing

Set these in the shell before running examples / REPL / cost benches. Code reads the base URL from the env; the key is passed as `Config.APIKey` (hosts typically load it from `LLM_GATEWAY_KEY`).

| Variable | Required | Role |
|----------|----------|------|
| `LLM_GATEWAY_BASE_URL` | **Yes** | OpenAI-compatible base including `/v1` (e.g. `https://llm-gateway.example.com/v1`). Read by `NewGatewayProvider` via `provider.go` (`GatewayBaseURLEnv`). |
| `LLM_GATEWAY_KEY` | **Yes** (convention) | Bearer token for the gateway. Pass into `toroid.Config{ APIKey: os.Getenv("LLM_GATEWAY_KEY"), ... }`. |
| `TOROID_MODEL` | Recommended for harnesses | Full model id, e.g. `llmgateway/kimi-k2p6`. Examples may use this name; kernel field is `Config.Model`. |

Example:

```bash
export LLM_GATEWAY_BASE_URL='https://llm-gateway.example.com/v1'
export LLM_GATEWAY_KEY='sk-...'
export TOROID_MODEL='llmgateway/kimi-k2p6'

# Host / example:
#   Model:  os.Getenv("TOROID_MODEL")
#   APIKey: os.Getenv("LLM_GATEWAY_KEY")
```

Do **not** require `ANTHROPIC_API_KEY`, `GEMINI_TOKEN`, or `OPENAI_API_KEY` for this port’s acceptance tests.

---

## 5. Multimodal — in scope vs out

| In scope | Out of scope |
|----------|--------------|
| Image (+ PDF if already supported) on **user** messages | Image generation models |
| Optional images on **tool results** (e.g. screenshots) | Audio/video pipelines |
| Model `input` includes `image` check | “Support every vision model” |
| Max bytes / dimensions per part | Unlimited base64 in transcript |
| Persist portable paths for resume (already) | — |

Current code: `multimodal.go` markdown image refs. Port work = caps + capability metadata + events on failure.

---

## 6. Structured generation — first-class kernel API

**Product contract (keep):**

```go
kernel.Run(ctx, prompt, toroid.WithSchema(schema, name, description))
// or Stream(..., WithSchema(...))
```

**Semantics (keep unless explicitly changed):**

1. Run full agentic **chat** (tools allowed) as free text / tool loop.  
2. Append a user nudge to emit JSON.  
3. One **llm-step**: `GenerateObject` (or equivalent) over history.  
4. Return validated JSON (Run string / Stream writer).

**Required improvements for implementer:**

- [x] Bill and emit `EventTurnCost` for the object **llm-step** (`CompleteObject` + `recordUsage`).  
- [x] Include that **llm-step** in transcript totals (`runningCostUSD`).  
- [x] Document that object pass is part of the same **chat**, not a new transcript (code comments + terminology).  
- [x] Do not remove `WithSchema` or push schema-only to the host.

Optional later: native structured output on the **last** tool-loop **llm-step** (provider json_schema) to avoid a second call — only if quality/cost wins; keep API stable.

---

## 7. Cost & pricing (must align with standard_pricing.md)

Implementer must:

1. Put rates on the **model catalog** (M1) — can still load from `assets/pricing.json`.  
2. Never swallow pricing miss as `$0` without a flag/field.  
3. Price `cache_read` / `cache_write` when present.  
4. Roll up: `transcript.cost = Σ chat`, `chat.cost = Σ turn/llm-step` including compact + schema + subagent.  
5. Read [standard_pricing.md](./standard_pricing.md) for gateway hybrid (stream vs non-stream headers).

---

## 8. Suggested target interfaces (Go sketch)

Names can change; **behavior** is the contract.

```go
// One registered model (catalog row).
type Model struct {
	ID            string // e.g. "llmgateway/kimi-k2p6" only form in this phase
	Provider      string // "llmgateway"
	API           string // "openai-completions" only for this phase
	ContextWindow int
	Cost          ModelPricing // per-token USD; from pricing.json
	Reasoning     bool
	Input         []string // "text", "image", ...
	Compat        *OpenAICompat // openai-completions / LiteLLM quirks
}

type Context struct {
	System   string
	Messages []Message // user | assistant | tool — no duplicate system
	Tools    []Tool
}

type AssistantMessage struct {
	Content    []Block
	Usage      Usage // tokens + Cost + PricingOK bool
	StopReason string // stop | toolUse | length | error | aborted
}

// One LLM call = one product "llm-step".
type Step interface {
	Stream(ctx context.Context, model Model, c Context, opts StepOptions) (*StreamResult, error)
	Complete(ctx context.Context, model Model, c Context, opts StepOptions) (*AssistantMessage, error)
	CompleteObject(ctx context.Context, model Model, c Context, schema ObjectSchema, opts StepOptions) (*ObjectResult, error)
}
```

Kernel **chat** loop (pseudocode):

```text
for {
  msg = Step.Stream(model, context)          // one llm-step
  append assistant to history
  if msg.StopReason != toolUse { break }     // end CHAT (or final turn)
  run tools                                  // local; not an llm-step
  append tool results                        // completes TURN
  maybe prune / compact / budget
}
if WithSchema { Step.CompleteObject(...) }   // extra llm-step in same CHAT
```

---

## 9. Implementation phases

Status key: **[x]** done · **[~]** partial · **[ ]** not done

### Phase A — catalog & honesty (no Fantasy removal)

- [x] Model catalog with cost + contextWindow + modalities (`model.go` / `ResolveModel`; rates from `pricing.json`).  
- [x] Pricing miss → `Usage.PricingOK=false` (not silent free $0); flows via `TurnCostPayload.TurnUsage`.  
- [~] Cost accounting on compact **llm-step** + schema **llm-step** + subagent rollup — **schema billed via Step**; compact + agent steps still billed via Fantasy usage path.  
- [x] Multimodal size caps (5 MiB) + vision capability gate + warnings.  
- [~] Terminology in comments on new Step/catalog code; events still Fantasy-ish “step/turn” naming (no product `llm-step` event kind).

### Phase B — Step interface + Fantasy adapter

- [x] Define `Step` + `Context` + `Model` in-repo (`step.go`, `model.go`).  
- [x] Adapter: `FantasyStep` — one Fantasy Generate/Stream/GenerateObject per **llm-step** (not full Agent inside Step).  
- [~] Kernel: **schema path** uses `k.Step.CompleteObject`; **`streamCurrent` still Fantasy `Agent.Stream`** with catalog cost on `OnStepFinish`.  
- [ ] Prefer: kernel-owned tool loop calling Step once per turn (big control win) — **pending** (see §15).

### Phase C — openai-completions / LiteLLM depth (only if Fantasy is insufficient)

- [x] **Skipped intentional client rewrite** — Fantasy `openaicompat` sufficient for `llmgateway/*`.  
- [~] Compat + usage-in-stream + LiteLLM cost/session headers — session/trace headers + **non-stream** `x-litellm-response-cost`; stream remains local estimate.  
- [x] **Do not** add native Anthropic/Google clients in this phase (legacy paths still exist; not expanded).  
- [ ] Drop Fantasy for the gateway path when A+B+C cover production models — **not started** (and not required while Fantasy works).

### Phase D — polish

- [x] Faux Step for tests (`fauxstep.go`).  
- [x] onPayload debug (`StepOptions.OnPayload` / `PayloadDebug`) — on Step only; not a kernel Config knob.  
- [~] Cross-model handoff (`TransformForHandoff`) — **implemented + tested; not called from Compact/subagent**.  
- [x] Abort/partial continue on **Step** stream (`StopReasonAborted`); main agent loop still Fantasy cancel semantics.

**Do not** schedule native Anthropic/Google work under this document.  
Phase A honesty is landed — further Phase C depth is optional polish, not a blocker for catalog cost views.

---

## 10. Acceptance checklist (handoff definition of done)

| # | Criterion | Status |
|---|-----------|--------|
| 1 | **Terminology** in new docs/events consistently (transcript/chat/turn/llm-step) | **Partial** — docs + Step comments; event kinds unchanged |
| 2 | Production model ids resolve in catalog with rates **or** explicit unavailable | **Yes** (rates via pricing; meta table sparse — see §15) |
| 3 | `$` on EventTurnCost / RunningCostUSD trustworthy for table-backed models; no silent zero | **Yes** for table hits; `PricingOK` distinguishes miss |
| 4 | Structured generation via `WithSchema` and its **llm-step** is costed | **Yes** |
| 5 | Multimodal user images work; oversized inputs bounded | **Yes** |
| 6 | Provider scope: only invest in **`llmgateway/*`** this phase | **Yes** (legacy native providers remain callable) |
| 7 | No pi source tree, OAuth, or image-generation surface | **Yes** |
| 8 | Kernel still owns tools, subagents, compact, SQLite, OTEL | **Yes** |
| 9 | Live tests use gateway env; unit tests use faux Step | **Unit yes**; live harness unchanged / host-dependent |

A port is “done enough” for **merge of the library layer** when rows 2–8 hold. Full architectural preference (§8 kernel loop) is tracked under §15, not a merge blocker for honesty/schema work.

---

## 11. Anti-goals (reject in review)

- “Support all models pi supports.”  
- “Add native Anthropic/Google/OpenAI in this phase.”  
- “Replace Fantasy this week with a multi-provider stack.”  
- “Host must implement schema and multimodal outside the kernel.”  
- “Copy pi’s agent loop; delete toroid Kernel.”  
- “$0 means free” when the model is missing from the table.

---

## 12. File touch map (as implemented)

| Area | Files |
|------|-------|
| Catalog | `model.go`, `model_test.go` |
| Pricing honesty | `pricing.go`, `pricing_test.go`, `assets/pricing.json` |
| Step API | `step.go`, `step_test.go` |
| Fantasy adapter | `fantasystep.go` |
| Faux Step | `fauxstep.go` |
| Gateway cost header | `costcapture.go`, `costcapture_test.go`, `provider.go` |
| Handoff | `handoff.go`, `handoff_test.go` |
| Kernel (partial Step use) | `kernel.go` — `Kernel.Step`, schema via `CompleteObject` |
| Multimodal | `multimodal.go`, `multimodal_test.go`, `history.go` (call site) |
| Docs | this file, `terminology.md`, `standard_pricing.md`; **`ARCHITECTURE.md` not yet updated** |

All of the above live in package `toroid` (root module) — no separate `step/` package yet; that remains optional.

---

## 13. One-sentence mandate for the implementing model

**Build a pi-inspired, Go-native model catalog and (eventually) one-llm-step Step API for OpenAI-compatible LiteLLM only (`llmgateway/*`) — with first-class structured generation, bounded multimodal input, and honest cost — without forking pi, without other native providers, and without moving the tool loop out of the toroid Kernel. Test against `LLM_GATEWAY_BASE_URL` + `LLM_GATEWAY_KEY` + a `llmgateway/...` model id.**

---

## 14. Structure review (2026-07-10)

Review of branch `worktree-llm-step-port` (not main `master`) against this scope.

### Verdict

**The structure is correct and supports the port.**  
Layering matches §2: Host → Kernel (chat/tools) → Step (one llm-step) → openai-completions / LiteLLM via Fantasy. Tool loop was not moved out of the Kernel; pi was not vendored; provider zoo was not expanded.

What is incomplete is **wiring depth**, not file layout. Treat prior “all four phases landed” as: **library + schema path complete**; **preferred kernel Step loop + handoff integration still open**.

### Layer map (actual)

```text
Host
  └─ Kernel (kernel.go: history, tools, compact, queue, subagents, events)
       ├─ Fantasy Agent.Stream  ← main chat loop today (multi-step inside Fantasy)
       └─ Kernel.Step (FantasyStep by default)
            ├─ CompleteObject  ← WithSchema path (costed) ✓
            ├─ Complete / Stream  ← available; not used by streamCurrent yet
            └─ protocol: openaicompat → LLM_GATEWAY_BASE_URL (llmgateway)
```

| Layer | Owns | Worktree reality |
|-------|------|------------------|
| Kernel | Tool loop, turns, compact, subagents, events, store | Still true; `Agent.Stream` + `OnStepFinish` cost |
| LLM Step | One llm-step | `Step` interface + `FantasyStep` / `FauxStep` |
| Protocol | OpenAI-compat + LiteLLM | `NewGatewayProvider` + `traceTransport` + cost header capture |

### Feature matrix (M1–M12) vs code

| ID | Feature | Status |
|----|---------|--------|
| M1 | Model as data | **Done** — `ResolveModel` |
| M2 | Usage + cost honesty | **Done** — `PricingOK` |
| M3 | Caller-owned Context | **Done** on Step path |
| M4 | Step-only API | **API done**; **kernel loop partial** |
| M5 | Single wire llmgateway | **Done** for port intent; native prefixes still registered |
| M6 | Prompt cache / gateway-honest | **Partial** — stream estimate; non-stream header preferred |
| M7 | Structured generation costed | **Done** |
| M8 | Multimodal caps + capability | **Done** |
| M9 | Cross-model handoff | **Library only** (not wired) |
| M10 | Request debug | **Done** on Step; not host Config |
| M11 | Abort + partial | **Done** on Step stream |
| M12 | Faux / scripted model | **Done** |

### What fits well

1. **Ownership split** — Kernel injects `k.Step = NewFantasyStep(model)`; tests swap `FauxStep`.  
2. **M7 gap closed** — schema was previously unbilled; now `CompleteObject` + `recordUsage`.  
3. **Honesty is structural** — miss is `PricingOK=false`, not a UI-only hint.  
4. **Phase C choice matches scope** — no home-grown client while Fantasy works.  
5. **Tests prove the target loop** — `TestKernelOwnedLoopOverStep` exercises Step-per-turn outside Fantasy Agent.

### Gaps (structure OK, integration incomplete)

1. **Preferred Phase B path not taken** — `streamCurrent` still `fantasy.NewAgent(...).Stream`; Step is not per-turn.  
2. **Compact still Fantasy `Agent.Generate`** — bills via `FromFantasyUsage`, misses Step cost-sink path.  
3. **`TransformForHandoff` is dead at runtime** — low risk while all in-scope models share openai-completions; still not integrated.  
4. **`modelMetaTable` is thin** — unknown ids (e.g. many `kimi-*`) get text-only + `ContextWindow=0` even when pricing resolves.  
5. **Doc drift** — `ARCHITECTURE.md` still pure Fantasy-loop story; §9 checkboxes were stale (now updated).  
6. **Default model** remains `anthropic/claude-haiku-4-5` (legacy), not gateway-first.

### Anti-goals integrity

Preserved: no pi tree, no OAuth/image-gen surface, tools stay in Kernel, no multi-provider stack rewrite, no silent free $0.

---

## 15. Pending work

Ordered roughly by leverage. None of this requires reshaping the package tree.

### P0 — keep honesty trustworthy in production hosts

- [x] Ensure hosts / REPL surface **`PricingOK` / “pricing unavailable”** (not only `$0.00`) when the table misses. — REPL already probes and prints "pricing unavailable"; honesty is structural via `Usage.PricingOK`.  
- [x] Expand **`modelMetaTable`** (or equivalent) for production gateway ids: at least context window + vision. — Refactored to family-prefix fallback (`lookupModelMeta`): all Claude / GPT-5 / Gemini variants plus common third-party gateway families (kimi, glm, deepseek, qwen) resolve a context window; vision reflects the base family.  
- [ ] Confirm **`pricing.json`** covers every production model id the host uses (or accept explicit unavailable). — *Host-specific; deliberately NOT inventing rates for kimi/glm/etc. They resolve as `PricingOK=false` (honest unavailable). Add real rates per deployment.*

### P1 — finish Step integration (preferred architecture)

- [x] **Kernel-owned tool loop:** drive each turn’s LLM call via `Step.Complete` instead of Fantasy `Agent.Stream` (preserves MaxIter, loop guard, queue interrupt, mid-turn compact, tool events, cost). — `steploop.go`, opt-in via `Config.UseStepLoop`; unit-tested with `FauxStep`. Default remains the proven Fantasy path pending live-gateway validation.  
- [x] Route **Compact** summarize through `Step.Complete` (gateway cost sink on non-stream). — `Compact` now uses `cheapStep().Complete`; billed via `recordUsage`.  
- [x] Optionally route other one-shot LLM paths through Step. — Only compact + schema exist; both now go through Step.

### P2 — handoff + polish

- [x] Call **`TransformForHandoff`** when Compact switches to `SmallerModel` (no-op for same-API). — Wired into `Compact`. *(Subagents start a fresh history, so there is nothing to transform there.)*  
- [x] Expose **OnPayload** via kernel Config for hosts. — `Config.OnPayload` threaded into every kernel Step call (schema + compact + step loop).  
- [~] Align **main-loop cancel** with Step abort semantics. — Step stream returns partial + `StopReasonAborted` on cancel; the opt-in step loop honours `ctx.Err()` between turns. The default Fantasy path's ESC/cancel behaviour is unchanged (Fantasy-owned).

### P3 — docs and naming

- [x] Update **`ARCHITECTURE.md`** for Kernel → Step → Fantasy/gateway. — Added the Step-layer section, gateway cost-capture note, and source-file entries.  
- [ ] Gradually rename comments/events toward **transcript / chat / turn / llm-step** (no big-bang rename). — *Ongoing; new code uses the terminology.*  
- [x] Keep this file’s phase boxes and §14 in sync when merging to main. — §15 boxes updated to reflect this branch.

### P4 — optional / later

- [ ] Live integration test harness with `LLM_GATEWAY_BASE_URL` + `LLM_GATEWAY_KEY` + `llmgateway/...`.  
- [ ] Drop Fantasy for gateway path only if Step + openaicompat depth fully replaces Agent needs.  
- [ ] Extract `step/` package if the root package grows too large (not required).

### Explicit non-goals (still)

- Native Anthropic / Google / OpenAI investment.  
- Provider zoo, OAuth, image generation, pi source port.  
- Moving tool execution into the Step package.
