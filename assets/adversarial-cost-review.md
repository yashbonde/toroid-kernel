# Adversarial Cost-Efficiency Review — toroid-kernel

**Scope:** token cost first, wasted CPU second. Adversarial assumptions: long chats
(50+ turns), large tool outputs, vision inputs, subagent-heavy workflows.
**Method:** read the actual source — `kernel.go`, `steploop.go`, `gatewaystep.go`,
`llm/client.go`, `llm/message.go`, `llm/tool.go`, `multimodal.go`, `history.go`,
`store.go`, `model.go`, `tools/*.go`, `prompt_compiler.go`. No speculation about
unread code.

---

## Executive summary

The kernel is disciplined about *tool schema* size (short one-line descriptions,
schemas generated once) and about sending the system prompt as a single leading
message. But the money leaks are structural and every one of them is paid **per
turn on a growing prompt**, so they compound in exactly the long-chat / vision /
subagent regimes this review targets.

The four biggest leaks:

1. **No prompt-cache breakpoints are ever emitted.** `buildBody` sends plain
   OpenAI-compatible JSON with no `cache_control`. The default model is
   `claude-haiku-4-5` (Anthropic family), which does **not** cache without explicit
   breakpoints. So the entire growing prompt is re-billed at full input price every
   turn instead of ~10% cache-read price. This is the dominant cost in any chat past
   a few turns.

2. **Pruning rewrites already-sent history in place every user turn**
   (`pruneOldToolCalls` → `trimHistoryRange`), which busts whatever prefix cache
   *did* exist. It is cache-hostile by construction and fires early (20k-token
   budget).

3. **Media (images/PDFs) is re-sent, re-base64-encoded, and re-billed every turn,
   uncapped, and is never pruned.** The read tool attaches media with no size cap
   and no vision-capability gate; `trimHistoryRange` never strips `Files`.

4. **Cost data is silently lost in two places:** subagent spend is rolled up as only
   the child's *last* llm-step (not its total), and streaming llm-steps carry no cost
   and have no pricing fallback. Both under-report `RunningCostUSD`, defeating any
   cost envelope built on it.

Fixing #1–#3 is most of the dollar savings; #4 is a correctness/billing-integrity
issue that can mask runaway spend.

---

## Ranked findings

| # | Finding | File:line | Class | Est. impact |
|---|---------|-----------|-------|-------------|
| 1 | No `cache_control` breakpoints → zero prompt caching on Anthropic-family default model | `llm/client.go:324-384` | cache | **5–10× input $** per chat |
| 2 | `pruneOldToolCalls` mutates old history in place every user turn → cache-prefix bust | `kernel.go:567,670-720` | cache | full-tail recompute per prune |
| 3 | Media re-sent/re-encoded/re-billed every turn; uncapped; never pruned | `llm/client.go:465-487`, `kernel.go:691-720`, `tools/read.go:106-117` | tokens+CPU | image tokens ×N turns |
| 4 | Subagent cost rolled up as last step only → massive undercount | `kernel.go:826,838-847`, `kernel.go:401-416` | lost $ data | subagent spend ≈ unbilled |
| 5 | Streaming llm-steps unbilled (no cost header, no pricing fallback) | `gatewaystep.go:152-157`, `kernel.go` (no pricing) | lost $ data | any streamed step = $0 |
| 6 | `read` tool text output uncapped (≤ 2000×2000 ≈ 4 MB) re-sent every turn | `tools/read.go:122-150` | tokens | up to ~1M tok/read in history |
| 7 | Pruning silently dies after first compaction (stale `StepHistoryStart` indices) | `kernel.go:668-720`, `Compact` 771-787 | tokens | no pruning post-compaction |
| 8 | Structured-output pass is an extra full-context llm-step | `kernel.go:579-600` | redundant step | +1 full prompt per schema Run |
| 9 | `subagent` tool output uncapped → full transcript re-sent every turn | `tools/subagent.go:16-20` | tokens | unbounded tool result |
| 10 | `EventAssistantTurn` persists full message JSON (base64 images + reasoning) to SQLite every turn | `steploop.go:139-141`, `store.go:213-223` | disk growth | DB bloat ∝ media×turns |
| 11 | `StepUsage`/`StepHistoryStart` grow unbounded, never reset at compaction | `kernel.go:53,668-673`, `Compact` | memory+CPU | O(steps) walk each turn |
| 12 | Loop-guard signature concatenates full tool-result text each turn | `steploop.go:187` | CPU | large alloc/turn |
| 13 | `Sessions[self]` overwritten with last step, not summed → Stop/Run usage undercounts | `kernel.go:404-408,626-633` | lost $ data | per-session tokens wrong |
| 14 | Compaction summarize pays for full un-stripped media; `trimHistoryRange` skips `Files` | `kernel.go:730,691-720` | tokens | image $ in summarize call |
| 15 | System-prompt date changes across days (cross-session cache/no per-turn) | `prompt_compiler.go` | cache (minor) | new prefix per day |

---

## Detailed findings

### 1. No prompt-cache breakpoints — the whole prompt is re-billed at full price every turn

**Where:** `llm/client.go:324-384` (`buildBody`). The body is `model`, `messages`,
`tools`, optional `tool_choice`/`response_format`/`reasoning_effort`/`max_tokens`.
There is **no `cache_control`** anywhere, and no `extra_headers`/`extra_body` to ask
LiteLLM to inject Anthropic cache breakpoints.

**Mechanism:** The default model is `claude-haiku-4-5` (`kernel.go:73`), an Anthropic
model reached via LiteLLM. Anthropic prompt caching is **opt-in**: without
`cache_control` breakpoints on the system block and the last stable message, nothing
is cached. OpenAI models auto-cache long prefixes, but the *default* config here is
Claude, so out of the box **every turn re-charges the entire prompt as fresh input.**
`wireUsage.toUsage` (`client.go:549-569`) will faithfully report `CacheRead=0` — the
telemetry will confirm the leak but the code never tries to cause a cache hit.

**Cost impact (adversarial):** A 50-turn agentic chat that grows to ~150k prompt
tokens averages, very roughly, ~75k prompt tokens/turn × 50 = ~3.75M input tokens.
With proper caching ~90% of that is cache-read. At haiku-class pricing (~$1/M fresh
vs ~$0.1/M cache-read) the difference is roughly **$3.4 vs ~$0.7 per chat on the
input side alone — a 5×+ multiplier**, and larger on Sonnet/Opus. This is the single
biggest lever in the codebase.

**Fix:** In `buildBody`, when the target family supports it, add
`cache_control: {type: "ephemeral"}` to (a) the system message and (b) the last
content block of the last "stable" message (typically the final tool result of the
prior turn), i.e. the standard "cache the prefix, leave the tail live" pattern.
Through LiteLLM this is done by tagging blocks on the OpenAI-shaped message or via
`extra_body`. Gate on `ResolveModel(...).Provider`/family so OpenAI routes (which
auto-cache) are left alone. This one change is worth more than all the others
combined.

---

### 2. `pruneOldToolCalls` rewrites already-sent history in place → cache-prefix bust

**Where:** called at `kernel.go:567` (once per user prompt, before the loop);
implementation `kernel.go:670-720`. `trimHistoryRange` sets old `ToolCallPart.Arguments`
to `"{}"` and truncates old `ToolResultPart.Content` **in place** on `k.History`.

**Mechanism:** Whatever prefix cache exists (finding #1 fixed, or OpenAI auto-cache)
depends on the byte-for-byte prefix being identical to the previous request. Trimming
a message that was sent in full last turn changes the prefix at that point, so
**everything from the first trimmed message onward is a cache miss** on the next turn.
The trigger budget is only `ToolCallPrunedSize = 20_000` tokens (`kernel.go:118`), so
in real tool-heavy work this fires almost every turn, repeatedly re-busting the prefix
as more content crosses the threshold.

**Cost impact:** Each prune event forfeits cache on the entire post-trim tail for that
turn. In a chat where pruning triggers every few turns, you pay full uncached input on
a large tail repeatedly — this can *erase* the savings from finding #1's fix if not
coordinated.

**Fix:** Two options: (a) make pruning **append-only** — never mutate messages already
sent; instead drop/replace whole old turns only at compaction boundaries (where a cache
bust is already unavoidable). (b) If in-place trimming is kept, trim only *once* per
message and only messages that fall *before* the cache breakpoint, and raise
`ToolCallPrunedSize` substantially so it does not thrash. The current per-turn in-place
rewrite is the worst case for caching.

---

### 3. Media re-sent, re-base64-encoded, and re-billed every turn — uncapped, never pruned

**Where:**
- Wire encode every turn: `llm/client.go:465-487` (`toContentBlocks` calls
  `base64.StdEncoding.EncodeToString(v.Data)` for each `FilePart` on every request).
- Never stripped by pruning: `trimHistoryRange` (`kernel.go:691-720`) only clears
  tool-call args and truncates tool-result *text*; it never touches `ToolResultPart.Files`
  or user-message `FilePart`s.
- Read tool has **no media size cap and no vision gate**: `tools/read.go:106-117`
  does `os.ReadFile(path)` for any image/PDF and attaches it as `Files` regardless of
  size or whether the model supports images. (Contrast `multimodal.go:103-106`, which
  caps *markdown-ref* media at 5 MiB — the read tool bypasses that entirely.)

**Mechanism:** A `FilePart` lives in `k.History` for the life of the (pre-compaction)
conversation. Every turn, `buildBody` walks the whole history and re-emits each image
as a fresh `image_url` data URI. The image's vision tokens are re-billed every turn
(and, per finding #1, uncached), and the base64 string is recomputed every turn.

**Cost impact (adversarial):** One 5 MiB screenshot over 30 subsequent turns =
~6.7 MB of base64 re-serialized 30× (~200 MB of wasted marshaling) **and** the image's
input tokens (up to ~1.5k tok for a large image, more for multi-tile) re-billed 30×.
A read-tool PDF has no 5 MiB cap at all — a 20 MB PDF becomes a permanent per-turn
payload. This is the worst offender for any vision or screenshot-driven session.

**Fix:**
- Cap and gate media in the read tool: refuse/skip attaching media above a byte cap
  and when `ResolveModel(model).SupportsImage()` is false (reuse `multimodal.go`'s
  logic).
- In pruning/compaction, **strip `Files` from old tool results** (replace with a
  `"[image omitted]"` text stub) once they age out of a small recency window — images
  are almost never needed more than a turn or two after they are read.
- Cache the base64 encoding on the `FilePart` (encode once, reuse) to kill the
  per-turn CPU re-encode.

---

### 4. Subagent cost rolled up as the child's *last step only* — spend is nearly unbilled

**Where:** `kernel.go:826` (`output, usage, err := subKernel.Run(...)`) and the rollup
`kernel.go:838-847`, combined with `UpdateUse` at `kernel.go:401-416`.

**Mechanism:** `UpdateUse` does `k.Sessions[self] = u` — an **assignment**, not a sum
(`kernel.go:405`). So a kernel's `Sessions[selfID]` always holds only its *most recent*
step's usage. `Run` returns `UsagePayload{Tokens: Sessions-snapshot}` (via the Stop
event, `kernel.go:626-633`). The parent then rolls up with
`k.runningCostUSD += childU.Cost` (`kernel.go:841`) using that map — i.e. it adds only
the child's **final llm-step cost**, discarding every earlier step the subagent ran.
The child's correctly-summed `runningCostUSD` is never propagated.

**Cost impact:** A subagent that runs 15 llm-steps contributes only its 15th step's
cost to the parent's `RunningCostUSD`. In subagent-heavy or background-task workflows
(`SpawnBackground` → `RunSubagent`), the vast majority of real spend is invisible to
the parent's accounting. Any cost envelope/guardrail keyed on `RunningCostUSD` will
under-count and allow runaway spend.

**Fix:** Return the child's total cost explicitly (add a `TotalCostUSD float64` to
`UsagePayload`, populated from `k.runningCostUSD` at Stop) and roll *that* up, instead
of summing a per-session map that only holds the last step. Separately, fix
`UpdateUse` to accumulate per-session (see finding #13).

---

### 5. Streaming llm-steps are unbilled — no cost header, no pricing fallback

**Where:** `gatewaystep.go:152-157` (`usageFrom(resp.Usage, nil)` — cost is
explicitly `nil` on the stream path) and `client.go:136-138` (the gateway cost header
is deliberately not read on streams). There is **no local pricing table** to fall back
on — `pricing.go`/`pricing.json` were removed (per repo status), so `Usage.Cost`
stays `0` and `PricingOK` stays `false` for any streamed step.

**Mechanism:** The production main loop uses `Step.Complete` (non-stream) so it does
carry cost — good. But `GatewayStep.Stream` exists and is a public Step method; any
host or code path that streams gets **$0 recorded** for that llm-step with no warning.
Tokens are captured (`stream_options.include_usage`) but never priced.

**Cost impact:** Every streamed step is silently free in the ledger. Combined with
finding #4, the accounting can badly under-report.

**Fix:** Either (a) restore a minimal local pricing fallback keyed on tokens for the
streaming path (LiteLLM does not send the cost header on SSE), or (b) after a stream,
issue a cheap `/spend` / cost lookup by `x-litellm-call-id` (already captured at
`client.go:264`) to reconcile cost. At minimum, emit a warning when a step is recorded
with `PricingOK=false` so unbilled steps are visible.

---

### 6. `read` tool text output is uncapped — up to ~4 MB per read, re-sent every turn

**Where:** `tools/read.go:122-150`. Unlike bash/grep/glob/ls (which route through
`TruncateToolOutput`, cap 20k chars), the read tool builds `content` directly with no
overall cap. Bounds are `DefaultReadLimit = 2000` lines and `MaxLineLength = 2000`
chars/line → a worst-case single result of **~4 MB** (~1M tokens).

**Mechanism:** That result enters `k.History` and, until it ages past the 20k prune
window, is re-sent at full size every turn (and uncached, per #1). The *most recent*
read is never pruned, so a big read right before a long tail sits in every subsequent
prompt.

**Cost impact:** A single `read` of a large minified/generated file can add hundreds of
thousands of tokens to *every* turn until compaction. This is a realistic footgun the
model can trigger itself.

**Fix:** Route the read tool's assembled `content` through `TruncateToolOutput` (or a
read-specific cap), and/or lower `DefaultReadLimit` × `MaxLineLength` worst case. Keep
the "use offset to continue" affordance so truncation is recoverable.

---

### 7. Pruning silently stops working after the first compaction (stale indices)

**Where:** `pruneOldToolCalls` (`kernel.go:668-720`) uses `StepHistoryStart` indices;
`Compact` (`kernel.go:771-787`) replaces `k.History` with a 2-message summary but
**does not reset** `StepUsage` or `StepHistoryStart`.

**Mechanism:** After compaction, `StepHistoryStart` still holds pre-compaction indices
(e.g. 40, 55, …) that now point far beyond the new 2–3 message history.
`len(StepHistoryStart) == len(StepUsage)` still holds, so the guard at `kernel.go:671`
passes, but `trimHistoryRange(start, end, …)` gets `start` ≫ `len(History)`; after
clamping `end` to `len(History)` the loop `for j := start; j < end` never executes.
Net result: **old-tool-call pruning becomes a no-op for the rest of the session.**
Tool results then accumulate unpruned until the next full compaction.

**Cost impact:** Loses the incremental-pruning savings for the entire post-compaction
lifetime of a long chat, pushing more weight onto the expensive full compaction and
inflating per-turn prompt size in between.

**Fix:** Reset `StepUsage` and `StepHistoryStart` (and re-seed them consistently with
the 2-message summary baseline) inside `Compact`. This also fixes the unbounded growth
in finding #11.

---

### 8. Structured-output pass is a whole extra full-context llm-step

**Where:** `kernel.go:579-600`. After the agent loop finishes, a `WithSchema` run
appends `"Now return your findings in the required JSON format."` to the **entire
history** and issues `CompleteObject` — a second full-context call.

**Mechanism:** The final answer is effectively paid for twice: once as the loop's last
turn, once as the schema pass over the whole transcript. For a large history this
doubles the most expensive (largest-prompt) call of the run.

**Cost impact:** +1 full-prompt input charge per schema-mode Run. On a 150k-token
transcript that is a full ~150k-token input step purely for reformatting.

**Fix:** When the last loop turn already produced the final text, ask for the JSON in
the **same** final turn (send the schema as the forced tool on the last call) instead of
a separate pass, or run the schema pass against a **compacted/relevant slice** rather
than full history. Even truncating the transcript for the reformat step saves most of it.

---

### 9. `subagent` tool output is uncapped

**Where:** `tools/subagent.go:16-20` returns `llm.NewTextResult(output)` with no
`TruncateToolOutput`.

**Mechanism:** A subagent's entire final message becomes a tool result in the parent's
history and is re-sent every turn (until pruned). Subagent outputs can be large
(research dumps), so this is an unbounded per-turn payload in the parent.

**Cost impact:** Proportional to subagent verbosity × remaining parent turns.

**Fix:** Route subagent output through `TruncateToolOutput` (or a larger, explicit
subagent cap), consistent with MCP results (`tools/mcp.go:183` already caps).

---

### 10. `EventAssistantTurn` persists full message JSON (base64 media + reasoning) to SQLite every turn

**Where:** `steploop.go:139-141` marshals the turn's messages and fires
`EventAssistantTurn`; `store.go:213-223` writes the event `data` blob to the `events`
table under `Save:true`.

**Mechanism:** The persisted payload includes `ToolCallPart` args, `ReasoningPart`
text, and — via `partToWire` (`llm/message.go:151-160`) — **base64 of any `FilePart`**.
So every image the model saw is duplicated into the SQLite `events` table, per turn it
appears in an assistant/tool message.

**Cost impact:** Not token cost, but unbounded on-disk growth and heavier
`AppendEvent`/`json.Marshal` work each turn. For vision sessions the DB grows by the
full media size repeatedly.

**Fix:** Strip or externalize `FilePart.Data` before persisting turn events (store a
reference/hash, not the bytes); optionally drop `ReasoningPart` from the persisted
payload since it is already excluded from wire replay.

---

### 11. `StepUsage` / `StepHistoryStart` grow unbounded and are walked every turn

**Where:** appended at `steploop.go:53,137`; walked backward every user turn in
`pruneOldToolCalls` (`kernel.go:675`). Never truncated; not reset at compaction
(finding #7).

**Mechanism:** O(steps) slices that only grow, plus a full backward walk each turn.
Small per-entry, but combined with the post-compaction staleness it is pure waste.

**Fix:** Reset at compaction (finding #7) and/or bound to a recent window.

---

### 12. Loop-guard signature concatenates full tool-result text every turn

**Where:** `steploop.go:187`:
`sig += call.Name + "\x00" + call.Arguments + "\x00" + resultText + "\n"`.

**Mechanism:** Builds a string containing the **full** tool-result text (up to the 20k
cap, or ~4 MB for a read result per finding #6) purely to compare against the previous
turns' signatures for the loop guard. Large alloc + copy per turn.

**Fix:** Hash the components (`sha256`/`fnv` of name+args+result) and compare hashes;
the guard only needs equality, not the text.

---

### 13. `Sessions[self]` is overwritten, not summed → Stop/Run usage under-reports tokens

**Where:** `kernel.go:404-408` (`k.Sessions[k.Cfg.SessionID] = u`), surfaced in the
Stop payload `kernel.go:626-633`.

**Mechanism:** The per-session token map holds only the **last** step's usage, so the
`UsagePayload.Tokens` returned by `Run`/emitted on `EventStop` reports one step's
tokens for the whole session. (Cost via `runningCostUSD` is summed correctly; the token
map is not.) This is the root cause of finding #4's subagent undercount.

**Cost impact:** Token accounting/telemetry per session is wrong (undercounts to a
single step); anything downstream that sums these maps is misled.

**Fix:** Accumulate: `s := k.Sessions[id]; s.add(u); k.Sessions[id] = s` (add token
fields and cost). Then finding #4's rollup becomes correct automatically.

---

### 14. Compaction pays for un-stripped media in the summarize call

**Where:** `Compact` calls `trimHistoryRange(0, len, 2000)` (`kernel.go:730`) before
summarizing, but `trimHistoryRange` never strips `Files` (finding #3). The summarize
llm-step (`kernel.go:754-760`) then sends the full history **including every image
blob** to the (often cheaper) summarizer.

**Cost impact:** The one call that is supposed to *reduce* cost re-pays for all media
in the transcript, at full size.

**Fix:** Strip `Files` (and reasoning parts) in the pre-compaction trim; the summary
does not need the raw images.

---

### 15. System-prompt date changes across days

**Where:** `prompt_compiler.go` (`time.Now().Format("2006-01-02")`).

**Mechanism:** The system prompt is built once per kernel (`kernel.go:272`), so this is
**cache-stable within a chat** — no per-turn variance (good). The day changes the
prefix across dates, so cross-day reuse is unavailable.

**Cost impact:** Minor — only matters across sessions/resumes, and only once cache is
enabled (finding #1).

**Fix:** Use date granularity (`2006-01-02`) so at least same-day sessions share a
prefix, or drop the time component.

---

## What is already done well (so it is not "fixed" away)

- **Tool schemas are compact and built once.** `toolDescription` in
  `prompt_compiler.go` returns short contracts, and `GenerateSchema` runs once in `NewTool`
  (`llm/tool.go:71-79`), not per turn.
- **System prompt is a single leading message**, never duplicated into history
  (`client.go:326-328`, comments at `kernel.go:527-531`), and compaction keeps it out
  of history — correct for a stable prefix.
- **Reasoning parts are dropped from wire replay** (`client.go:434-452` emits only
  text + tool_calls for assistant messages) — reasoning is not re-sent as input.
- **`wireTools` is computed once per `Stream`** (`steploop.go:27`), not per turn.
- **Most variable-length tool outputs are capped** at 20k chars via
  `TruncateToolOutput` (bash/grep/glob/ls/MCP). The gaps are the read tool (#6) and
  the subagent tool (#9).
- **Non-streaming main loop carries the authoritative gateway cost header**
  (`gatewaystep.go:74`, `client.go:112-116`) — the honest-cost design is sound where
  the cost header exists; the gaps are streaming (#5) and subagent rollup (#4).

---

## Recommended order of work (by dollar impact)

1. **Finding #1** — emit `cache_control` breakpoints for Anthropic-family routes.
   Biggest single win; without it everything else is a rounding error.
2. **Finding #2 + #3** — stop mutating sent history in place; strip/age-out media.
   These protect the cache win from #1 and cut vision cost.
3. **Finding #6 + #9** — cap read-tool and subagent outputs.
4. **Findings #4, #5, #13** — fix cost accounting so envelopes are trustworthy and no
   llm-step is silently unbilled.
5. **Findings #7, #8, #10, #11, #12, #14, #15** — pruning-post-compaction, schema-pass
   merge, persistence bloat, CPU cleanups.

---

## Resolution addendum (applied same day)

All 15 findings were addressed; status per finding:

| # | Status | Resolution |
|---|--------|------------|
| 1 | **Fixed** | `llm.Request.CachePrompt`: cache_control breakpoints on the system message + last user/tool message, gated per model family (`Model.PromptCache`, Claude only — OpenAI routes auto-cache). Live-verified on `claude-sonnet-4-6`: step 2+ read the full prior prefix from cache (`cache_read=3890`, `input=2` by step 4). One-shot steps (compaction, schema pass) skip stamping — different tool set means the prefix can never hit, and stamping would pay the cache-write premium for nothing. Note: this gateway's Bedrock-backed `claude-haiku-4-5` ignores cache_control upstream (no error, no caching) — a deployment limitation, not a client one. |
| 2 | **Fixed** | Per-turn `pruneOldToolCalls` removed entirely. History is never mutated mid-chat; trimming now happens only inside `Compact`, where the cache bust is unavoidable anyway. `Config.ToolCallPrunedSize` removed. |
| 3 | **Fixed** | Read tool refuses media over 5 MiB (`MaxMediaBytes`); the step loop strips tool-result media for non-vision models at execution time; compaction (`trimForCompact`) strips all `Files` + reasoning before the summarize call. Per-turn base64 re-encode remains (media is capped and stripped at compaction, bounding it). |
| 4 | **Fixed** | Root cause was #13: `UpdateUse` now accumulates per-session usage, so a subagent's `Sessions` entry is its true total and the parent rollup adds real spend. |
| 5 | **Mitigated** | No local pricing table by design (gateway-truth only). `recordUsage` now logs a warning whenever an llm-step is recorded with `PricingOK=false`, so unbilled steps are visible. The kernel loop is non-streaming, so production steps always carry the header. |
| 6 | **Fixed** | Read tool text output routed through `TruncateToolOutput` (20k cap). |
| 7 | **Fixed** | Obsolete — the stale-index pruning machinery (`StepUsage`/`StepHistoryStart`) was deleted with #2. |
| 8 | **Accepted** | The schema pass stays a separate llm-step: folding it into the final loop turn would require knowing the last turn in advance. Its input is paid once per `WithSchema` run and its usage is fully billed. |
| 9 | **Fixed** | Subagent tool output routed through `TruncateToolOutput`. |
| 10 | **Fixed** | `appendStepMessages` strips `FilePart.Data` from persisted `EventAssistantTurn` payloads (text stub instead); resume gets the stub, SQLite no longer stores base64 media. |
| 11 | **Fixed** | Slices deleted with #2/#7. |
| 12 | **Fixed** | Loop-guard signature is now an FNV-64a hash per call instead of concatenated full result text. |
| 13 | **Fixed** | See #4. |
| 14 | **Fixed** | `trimForCompact` strips media + reasoning before the summarize call. |
| 15 | **Fixed** | System-prompt date is now day-granular (`2006-01-02`). |
