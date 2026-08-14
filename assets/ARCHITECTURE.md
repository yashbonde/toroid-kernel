# toroid-kernel — Architecture Explainer

> An embeddable Go kernel for tool-using LLM agents.

Source: [github.com/yashbonde/toroid-kernel](https://github.com/yashbonde/toroid-kernel) · ~3,200 LOC Go · self-contained (in-repo [llm](../llm) package; no third-party model SDK) · reflects the code as of this update (2026-06-23).

> **What it is in one line.** A library — not a CLI or a TUI — that you `import` into a Go program to run a tool-calling agent loop with streaming, per-step cost accounting and hard spend limits, resumable persistence, OpenTelemetry-native tracing, conversation compaction, loop guards, structured output, image input, synchronous *and* background subagents, and a built-in tool registry. The host program owns the UI and the lifecycle; the kernel owns the loop.

This explainer walks the system as built (§1–§12): construction and wiring, the turn loop, tools, providers, the single SQLite persistence store, OTEL-native telemetry with an OTLP exporter, context management, the message queue, sub/background agents, and the event bus. §13 is the remaining roadmap — inter-kernel communication and permission gating — which is designed but not yet code.

---

## 1. The big picture

A host program constructs a `Kernel` from a `Config` and calls `Run` (buffered) or `Stream` (incremental). The kernel wires a *provider* (Google / Anthropic / OpenAI-compatible / LLM-gateway), a *tool registry*, an *event/hook bus*, and — when `Save` is set — a single embedded *SQLite store* that holds traces, spans, costs, events and memories (the todo table lives in the same database). The agent loop is kernel-owned (steploop.go): each turn is one llm-step performed by the Step layer over the in-repo OpenAI-compatible gateway client, with the kernel executing tools between steps and managing history, cost, compaction, loop guards, and the message queue.

```mermaid
flowchart TB
  host["Host program"]
  subgraph K["Kernel"]
    cfg["Config"]
    loop["Run / Stream<br/>agent loop"]
    hist["History []Message"]
    hooks["HookRegistry<br/>(event bus)"]
    reg["tools.Registry"]
    queue["messageQueue"]
  end
  fan["Step loop (steploop.go)"]
  prov["Provider<br/>google / anthropic / openai / llmgateway"]
  llm["LLM API"]
  sqlite[("SQLite store<br/>traces · spans · costs<br/>events · memories · todos")]
  otlp["OTLP exporter<br/>(Langfuse / any OTEL backend)"]

  host -->|"NewKernel(cfg)"| cfg
  host -->|"Run / Stream / Enqueue"| loop
  loop --> hist
  loop --> fan
  fan --> prov
  prov --> llm
  fan -->|"tool calls"| reg
  loop -->|"Fire(event)"| hooks
  hooks -->|"persisted events / costs / spans"| sqlite
  reg --> sqlite
  host -->|"On(kind, fn)"| hooks
  sqlite -.->|"OTELSpans / LangfuseOTLP"| otlp
  queue --> loop
```

## 2. Construction: `NewKernel`

[`NewKernel`](../kernel.go#L111) is where everything is resolved and wired, in order:

- **Defaults** — API key falls back to `GEMINI_TOKEN`; a Snowflake-based `SessionID` is generated; `WorkDir` defaults to a per-session runner dir; `ApplyDefaultDataTypes` fills the rest. The default model is `anthropic/claude-haiku-4-5`.
- **Trace root** — if `TraceID` is empty it is set equal to `SessionID`. This is the marker of a *root* kernel; subagents inherit the parent's `TraceID` instead.
- **Store** — opened only when `Save` is true. One shared SQLite handle backs both the trace store and the todo tools. On a non-root span it *reconstructs history* by replaying stored events (resume).
- **Tools** — `IncludeComputerTools` registers the five core file/shell tools. Skills, subagents, and MCP tools are added only when discovered or explicitly enabled. Host-supplied `Config.Tools` are merged in too.
- **System prompt** — `prompt_compiler.go` builds a small stable prefix after startup capabilities are known.
- **Provider + model** — resolved from the `provider/model` ID, then the `provider/` prefix is stripped before asking for the `LanguageModel`.
- **Loop guards** — two kernel-owned guards are wired into the step loop: `MaxIter` (default 25) caps the absolute number of tool-call turns, and `MaxRepeatCalls` (default 3) trips when consecutive steps issue identical tool calls with identical results. Both halt cleanly at a step boundary.
- **Spend limits** — `Config.MaxTranscriptSpendUSD` caps cumulative transcript cost; a caller can add `WithMaxTurnSpendUSD` to cap one `Run`/`Stream` call. Non-positive values disable the corresponding limit.
- **Thinking** — when `Thinking` is not `none`, a Google thinking-budget/level config is added (`low`≈1k, `high`≈8k tokens; gemini-3 uses thinking *levels*), and reasoning deltas can stream to `ThinkingWriter`.
- **Agent options** — system prompt, tools, and the optional thinking budget (gateway `reasoning_effort`) are applied per llm-step via `StepOptions`.

## 3. The turn: `Stream`

[`Stream`](../kernel.go#L503) runs one user turn: it fires `SessionStart` on the first turn, auto-compacts if near the limit, appends the (image-aware) user message, prunes old tool calls past budget, and then hands off to [`streamCurrent`](../kernel.go#L690), which owns the agent loop. Inside the loop, queued messages are injected and context pressure handled at each turn boundary: after a turn's tools run, the kernel drains the queue (appending queued user messages and continuing the same chat) or compacts when the window crosses the threshold — this is how mid-run input and mid-turn context overflow are absorbed without corrupting history. The loop is serialized by `runMu` so `Stream` and `Wake` (§9) never mutate history concurrently.

```mermaid
sequenceDiagram
  autonumber
  participant H as Host
  participant K as Kernel.Stream / streamCurrent
  participant F as Step loop
  participant L as LLM
  participant B as Hooks / Store

  H->>K: Stream(prompt, writer)
  alt first turn
    K->>B: Fire SessionStart
    K->>K: history += system msg
  end
  K->>K: auto-compact if near context limit
  K->>K: history += user msg (inline images)
  K->>K: prune old tool calls past budget

  loop restart on queue interrupt OR mid-turn compaction
    K->>F: Agent.Stream(history)
    loop each step
      F->>L: request
      L-->>F: tokens / reasoning / tool calls
      F-->>K: OnReasoningDelta -> Fire Reasoning
      F-->>K: OnToolCall -> Fire PreToolUse
      F-->>K: OnToolResult -> Fire PostToolUse(+/-)
      F-->>K: OnStepFinish -> cost, drain queue, check pressure
    end
    alt queue had messages
      K->>K: append steps + queued msgs
      K->>B: Fire QueueInterrupt
    else over context threshold
      K->>K: append steps, Compact (reset history)
    else done
      K->>K: append final steps
    end
  end

  K->>B: Fire AssistantTurn + Stop(usage) + (idle) MasterIdle
  K->>B: Store span end timestamp
  K-->>H: final text
```

### Per-step cost accounting

After every llm-step the wire usage plus the gateway-reported dollar cost (`x-litellm-response-cost`, present on non-streaming responses) is converted to a `Usage`, accumulated into `runningCostUSD`, appended to `StepUsage`, persisted via `AppendCost`, and emitted as an `EventTurnCost`. The turn boundary after tool execution is the **safe interruption point** where the message queue is drained and context pressure is re-checked.

`Usage` carries a `PricingOK` flag: it is `true` only when the gateway reported an authoritative cost for the call. There is no client-side pricing table — when the gateway does not report a cost, `Cost` stays `0` and `PricingOK` is `false`, so a surface can show "cost unknown" rather than a misleading free `$0`.

The step loop checks its call-local spend budget before each LLM request and
again after billing. Reaching `WithMaxTurnSpendUSD` or
`Config.MaxTranscriptSpendUSD` suppresses every later LLM step for the call,
including the structured-output pass and idle-kernel wake re-entry. The response
that crosses the boundary is necessarily billed because authoritative cost is
known only after the provider returns it.

### Structured output

`Run`/`Stream` accept a [`WithSchema(schema, name, description)`](../kernel.go) option. When set, the free-text loop output is discarded; after the agentic loop the kernel appends a "return your findings in the required JSON format" user turn and runs one structured-output **llm-step** through the Step layer (`Step.CompleteObject`), **billing its usage** via `recordUsage` and writing the validated JSON to the caller's writer instead of prose.

### The Step layer

A **Step** performs exactly one LLM call — one product *llm-step* (see [terminology.md](./terminology.md)). It never runs the tool loop; the kernel owns turns, tools, compaction, and subagents and drives a Step per call. The interface ([`step.go`](../step.go)) is `Complete` / `Stream` / `CompleteObject` over a caller-owned `Context` (single system blob + messages + tools).

| Type | Role |
|------|------|
| [`GatewayStep`](../gatewaystep.go) | Default backend: maps one llm-step onto a single OpenAI-compatible chat completion via the in-repo `llm.Client`. Prices usage from the catalog; on non-stream calls prefers the gateway's `x-litellm-response-cost` over the local estimate. |
| [`FauxStep`](../fauxstep.go) | In-memory, network-free backend for tests: scripted assistant messages (incl. tool calls) and fake usage. |

The Step layer backs every LLM call: the chat loop, the schema pass, and compaction. The **kernel-owned tool loop** ([`steploop.go`](../steploop.go)) drives each turn's LLM call through `Step.Complete` — executing tools in the kernel and preserving MaxIter, the repeat-call loop guard, queue interrupts, mid-turn compaction, tool events, and per-turn cost. It is the only chat loop; the former Fantasy `Agent.Stream` path is gone.

The model catalog ([`model.go`](../model.go), `ResolveModel`) is the "model as data" row — id, provider, wire `api`, context window, per-token cost, reasoning, and input modalities (used by the multimodal path's vision check).

## 4. Tools and the registry

Each tool is a `ToolDef` pairing a short compiled description with an
`llm.ToolHandler` (the executable, schema-bearing function). The registry is a
name-keyed map. Five core tools are enabled for ordinary coding runs:

| Tool | Purpose | Notable behavior |
|------|---------|------------------|
| `read` | Read file or list directory | Line numbers, 2000-line / 2000-char caps, binary detection, offset paging |
| `write` | Write a file | Creates parent dirs |
| `edit` | Exact-string replace | Fails if `oldText` is missing or non-unique |
| `multiedit` | Batched edits to one file | — |
| `bash` | Run a shell command | Non-interactive, 120s default timeout, output capped at 12k chars |

`skill` appears only when skills are discovered. `subagent` and
`subagent_async` require `IncludeSubagentTools`; MCP and host tools appear only
when configured.

> **Note.** Tool failures are returned as ordinary text beginning with `"Error:"`; in [`OnToolResult`](../kernel.go#L765) the kernel sniffs that string prefix to decide between `PostToolUse` and `PostToolUseFailure`. The result is now carried in a structured `ToolUseResultPayload` (with a dedicated `Error` field populated on the failure path), but the failure *detection* is still string-prefix based rather than a structured error flag from the tool.

### Image input

User prompts go through [`parseUserMessage`](../multimodal.go#L22), which scans for markdown image refs `![alt](path)`, loads each readable model-supported media file as an `llm.FilePart`, and splices it into the user turn as text→image→text parts. The persisted form of the prompt rewrites the path to a `~`-rooted absolute path so a session resumed from any directory still resolves the same file. Unresolvable refs are left as literal text.

## 5. Providers

Every model is reached through one wire: the in-repo [`llm.Client`](../llm/client.go), an OpenAI-compatible chat-completions client pointed at the LiteLLM gateway (`LLM_GATEWAY_BASE_URL`, bearer token from `Config.APIKey` / `LLM_GATEWAY_KEY`). Model ids of the form `llmgateway/<name>` have the prefix stripped on the wire; other ids pass through verbatim for the gateway to route. There are no native provider SDKs in the module.

The client stamps a per-chat `traceparent` + `x-litellm-session-id` on every request (trace id carried in the context via `WithGatewayTrace`) so a chat's turns nest under one upstream trace, and reads `x-litellm-response-cost` from non-streaming responses so every llm-step bills the gateway's authoritative cost. Streaming responses omit that header by design; their `Usage` carries tokens only. Transient failures (network, 429, 5xx) retry up to three times with backoff.

## 6. Persistence: one SQLite store

Persistence is consolidated on a **single embedded SQLite database** (`~/.swarmbuddy/sql.db`, opened with WAL + busy-timeout, via the pure-Go `modernc.org/sqlite` driver — no cgo). One process-global handle is shared between the trace [`Store`](../store.go) and the todo tools, so there is no inter-store lock contention. The [schema](../store.go#L49) is five tables:

| Table | Holds | Key / index |
|-------|-------|-------------|
| `traces` | Per-trace metadata (title, start/end, previous trace id) | `trace_id` |
| `spans` | Per-span (kernel session) metadata (parent, model, start/end) | `(trace_id, span_id)` |
| `costs` | Per-turn cost rows (turn + cumulative USD) | `(trace_id, span_id, ts)` |
| `events` | Full-fidelity session events as JSON | `(trace_id, span_id, seq)` |
| `memories` | Per-span agent memory JSON blob | `span_id` |

Every kernel is a **span**; the tree of kernels sharing a `TraceID` is a **trace**. A root kernel has `TraceID == SessionID`; a subagent inherits its parent's `TraceID` and sets `ParentSpanID` to the parent's `SessionID`. Span (and, for the root, trace) **start and end timestamps** are recorded via `SaveSpanMeta`/`SaveTraceMeta`, and all events except the high-volume display-only `Reasoning` are appended. `Close()` checkpoints the WAL but intentionally leaves the shared handle open for other live kernels; it is released on process exit.

```mermaid
flowchart TD
  subgraph trace["Trace  (TraceID = T1)"]
    root["Root kernel<br/>SpanID = T1<br/>ParentSpanID = (none)"]
    sub1["Subagent A<br/>SpanID = S2<br/>ParentSpanID = T1"]
    sub2["Subagent B<br/>SpanID = S3<br/>ParentSpanID = T1"]
    sub1a["Subagent A.1<br/>SpanID = S4<br/>ParentSpanID = S2"]
  end
  root --> sub1
  root --> sub2
  sub1 --> sub1a
  store[("SQLite<br/>events / costs / spans<br/>keyed by (TraceID, SpanID)")]
  root -.->|append| store
  sub1 -.->|append| store
  sub2 -.->|append| store
  sub1a -.->|append| store
```

### Resume

On a saved non-root span, `ReconstructHistory` replays stored events (only those after the last compaction) to rebuild the in-memory `History` before continuing — so a process restart can pick up an existing conversation. `LoadTraceData` / `ListSessions` / `DeleteSession` expose the stored graph for visualization and management.

## 7. Context management

Two independent mechanisms keep the context window in budget:

- **Auto-compaction** — checked both at the *start* of a turn and at each in-loop step boundary (a long agentic turn can balloon the window mid-stream). When `currentTokens ≥ TotalContextSize − CompactionBufferSize`, `Compact` asks the model to summarize the conversation, resets `History` to *[system, "tell me the summary", summary-as-assistant]*, resets the occupancy gauge, and emits `PreCompact`/`PostCompact` (the latter carries the summary and before/after diff so event-based resume can rebuild from the summary forward).
- **Tool-call pruning** — independently, before each turn the kernel walks `StepUsage` backward accumulating tokens; once a step's cumulative total exceeds `ToolCallPrunedSize` (default 40k), that step's history is trimmed in place: tool-call args become `{}` and tool-result text is clipped to ~800 chars.

Occupancy is tracked as *context-window* tokens (a single step's input-side + output tokens via `windowTokens`), never the turn's summed billed tokens — summing every step would double-count re-read context and wildly over-state how full the window is.

```mermaid
flowchart TD
  A["Turn begins"] --> B{"currentTokens >=<br/>TotalContextSize - CompactionBufferSize ?"}
  B -- yes --> C["Compact: summarize,<br/>reset history,<br/>fire Pre/PostCompact"]
  B -- no --> D["append user message"]
  C --> D
  D --> E["walk StepUsage backward,<br/>accumulate tokens"]
  E --> F{"cumulative ><br/>ToolCallPrunedSize ?"}
  F -- yes --> G["trim that step:<br/>args -> {} ,<br/>results -> 800 chars"]
  F -- no --> H["keep step intact"]
  G --> I["run agent loop<br/>(also compacts mid-turn if it overflows)"]
  H --> I
```

> **Note.** `Compact` rebuilds history with the summary stored as an *assistant* message that was manually relabeled from a user message — a shortcut that some stricter provider APIs may reject.

## 8. The message queue

`Enqueue(msg)` is safe to call from any goroutine while `Stream` is running. The message lands in `messageQueue`; at the next `OnStepFinish` the queue is drained, a flag is set, the current stream is allowed to finish so all step messages are collected, and then the loop restarts with the queued messages appended as user turns. This is the same seam that background agents (§9) use to wake an idle kernel on external completion.

## 9. Subagents and background agents

[`RunSubagent`](../kernel.go#L958) clones the parent `Config`, assigns a fresh `SessionID`, inherits the `TraceID`, sets `ParentSpanID` to the parent's session, builds a brand-new `Kernel`, and runs it **synchronously** to completion. `SubagentStart`/`SubagentStop` events bracket the call, and the child's usage map flows back into the parent's accounting. It is a single flat delegation primitive — no agent types, no tool restriction, no model override. The `subagent` tool exposes this to the model.

**Background agents (built).** The `subagent_async` tool calls [`SpawnBackground`](../kernel.go#L1042), which runs `RunSubagent` on a goroutine and returns a short task id immediately. When the child finishes it fires `EventTaskCompleted`, `Enqueue`s its result, and — if the parent loop has already gone idle — calls `Wake` to re-enter the loop and process the result, exactly the way a background-task completion notification wakes the agent in Claude Code. This reuses the message-queue + step-boundary-interrupt machinery (§8); the only genuinely new primitive is [`Wake`](../kernel.go#L1068), guarded so a live loop drains the queue itself and an idle kernel is re-entered with its last writer.

```mermaid
sequenceDiagram
  autonumber
  participant M as Model (parent loop)
  participant K as Parent kernel
  participant W as Background subagent

  M->>K: subagent_async(task)
  K->>W: SpawnBackground -> goroutine
  K-->>M: task id (returns immediately)
  M->>K: continue or finish turn
  Note over K: kernel may go idle
  W-->>K: completed -> Fire TaskCompleted + Enqueue(result)
  alt parent loop still running
    Note over K: drains queue at next step boundary
  else parent idle
    W->>K: Wake -> re-enter loop with result
  end
  K->>M: model processes result
```

## 10. Events and hooks

The kernel is observable through a simple synchronous bus. `On(kind, fn)` registers a hook; `Fire` stamps an [`Event`](../events.go#L30) with session/trace/span IDs and a sequence number, persists it (unless it is a `Reasoning` event), then runs every matching hook in order — a non-nil error aborts the chain. Each event also has a single canonical `OTEL()` projection and an `Observable()` flag (§11). The host uses these to render output, stream tokens, show cost, and react to lifecycle transitions.

| Category | Event kinds |
|----------|-------------|
| Lifecycle | `SessionStart`, `UserPromptSubmit`, `TurnStarted`, `TurnCompleted`, `TurnFailed`, `Stop`, `SessionEnd`, `MasterIdle` |
| Streaming | `Reasoning` (display-only thinking deltas; not persisted) |
| Tools | `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `PermissionRequest` |
| Subagents / background | `SubagentStart`, `SubagentStop`, `TaskCompleted` |
| Cost / context | `TurnCost`, `PreCompact`, `PostCompact`, `AssistantTurn` |
| Other | `Notification`, `QueueInterrupt`, `TraceLog` |

> **Worth knowing.** `MasterIdle` and `TaskCompleted` *are* now fired — they drive the idle/wake handling for background agents (§9). The one event still defined but never fired is `PermissionRequest`: it is scaffolding for the permission-gating capability the kernel does not yet enforce (§13.2). (`AssistantTurn` carries the full structured content blocks — thinking + text + tool_use — for a turn; the old high-volume `Token` event and the `Title` event have been removed.)

## 11. Telemetry: OTEL-native, OTLP export

OpenTelemetry is the kernel's telemetry substrate, not an afterthought. The persisted trace/span graph maps directly onto OTEL and there is **one canonical OTEL shape**, derived on read from the full-fidelity events stored under `Save:true`: [`OTELSpans`](../store.go#L541) projects each kernel span into an `OTELSpan` with spec-valid IDs, start/end timestamps, GenAI semantic-convention attributes (`gen_ai.request.model`, `gen_ai.usage.*_tokens`, `gen_ai.usage.cost_usd`), and the kernel's events as span events (filtered to `Observable()` kinds, dropping reasoning/idle/queue chatter). Because the projection is the single source of truth, the stored and exported views can never drift.

**ID strategy (the Snowflake design, implemented).** Span IDs are 64-bit [Snowflake IDs](https://en.wikipedia.org/wiki/Snowflake_ID) — `[42b ms | 12b node | 10b sequence]` ([utils.go](../utils.go#L206)) — which are time-ordered (so the SQLite indices stay cheap and spans sort chronologically for free) and are exactly the size of an OTEL span ID. `NewSessionID` encodes a Snowflake as 16 hex chars. [`OTELIDs`](../utils.go#L291) derives spec-valid OTEL IDs: the 64-bit Snowflake is the span ID, and the 128-bit trace ID is the trace's Snowflake high half plus a deterministic FNV-derived low half (so the same `traceID` always maps to the same OTEL trace ID).

**Export.** [`otlp.go`](../otlp.go) implements a minimal OTLP/HTTP-JSON exporter — it builds the OTLP request, gzip-encodes it, and POSTs to any OTLP backend. [`LangfuseOTLP(ctx, traceID, baseURL, publicKey, secretKey)`](../otlp.go#L491) is the ready-made entrypoint that ships a stored trace to Langfuse. The kernel takes no hard dependency on the OpenTelemetry SDK; a host calls the exporter (or feeds `OTELSpans` snapshots to its own exporter) whenever it wants to publish.

**Always-on file transcript.** Independent of `Save`/SQLite, every session appends its observable events to `~/.toroid/sessions/<session-id>/transcript.jsonl` ([transcript.go](../transcript.go)) — one OTEL span-event per line (spec-valid trace/span IDs from `OTELIDs` + the same `Event.OTEL()` projection used for export, so the file never drifts from the SQLite/exported views). It is best-effort (a write failure never disrupts a run) and filtered to `Observable()` kinds. This gives a durable, greppable trace of every run — including all tool/CLI commands — even when a host never enables the store. `SessionDir(sessionID)` returns the per-session directory (shared with the `tool-output/` overflow files).

## 12. Mental model in one diagram

```mermaid
flowchart LR
  cfg["Config"] --> nk["NewKernel"]
  nk --> ready["Kernel ready"]
  ready -->|"Run / Stream"| turn["Turn"]
  turn --> compact["maybe compact + prune"]
  compact --> agent["Step loop"]
  agent --> tools["tool calls"]
  tools --> agent
  agent --> events["events + cost"]
  events --> host["host hooks"]
  events --> store["SQLite store"]
  store --> otlp["OTLP / Langfuse"]
  turn -->|"Enqueue / Wake"| turn
  agent -->|"subagent / subagent_async"| child["child Kernel (same trace)"]
```

## 13. Roadmap (designed, not yet code)

Everything in §1–§12 is the system *as built* — including the storage consolidation, OTEL-native telemetry, and background agents that earlier drafts of this doc listed as future work. The two items below are the larger follow-on still to come. They are sequenced: permission gating (§13.2) is a prerequisite for safely opening the kernel to inter-kernel traffic (§13.1).

### 13.1 Inter-kernel communication (IKC)

The longer-range direction is **IKC** — letting separate kernels, in different processes or on different machines, delegate to each other. There is no `ikc/` package or `Serve` entry point yet; this is design, not code. IKC is the capability; the wire format is pluggable, and a **bespoke HTTP/JSON protocol** is the first example (a future A2A-compatible transport is another). The span model already propagates across the boundary: a remote kernel adopts the caller's `TraceID` and sets `ParentSpanID` to the caller's span, so one trace spans both processes, and the remote usage map merges back into the caller's cost accounting.

**Three delegation modes — same trace, different boundary.** IKC is the third rung of a ladder whose first two rungs now exist:

| Mode | Boundary | Timing | Status |
|------|----------|--------|--------|
| `subagent` (§9) | In-process child kernel | Synchronous — the call blocks | Built |
| `subagent_async` / background (§9) | In-process / local, async | Returns a task id; wakes caller on completion | Built |
| **IKC (§13.1)** | Another process / machine, over the network | Async like a background agent, plus auth, discovery, and trace propagation across the wire | Planned |

An IKC delegation would behave to the model exactly like a background agent (§9) — fire, get a task id, get woken on completion — the only difference being a worker behind a network hop with its own identity and trust boundary.

**Cluster, not peers.** The intended client UX is *dial into a cluster*, never "address a specific peer." A caller asks the cluster for a capability; a **discovery service** resolves that to a concrete kernel and the addressing is hidden. The cluster's only required job is discovery/registration — routing, the task stream, and trace propagation still flow kernel-to-kernel.

```mermaid
flowchart LR
  client["Caller kernel"]
  subgraph cluster["IKC cluster"]
    disc["Discovery / registry<br/>(resolve capability -> kernel)"]
    k1["Kernel A<br/>(serving, opt-in)"]
    k2["Kernel B<br/>(serving, opt-in)"]
  end
  client -->|"1. need capability X"| disc
  disc -->|"2. resolve -> Kernel B"| client
  client -->|"3. delegate task (direct)"| k2
  k2 -.->|"4. stream result + trace"| client
  k1 -.->|register| disc
  k2 -.->|register| disc
```

- **Serving is opt-in.** A kernel would not accept inbound tasks by default — exposing it requires an explicit toggle (`Serve` / a config flag). A kernel that only delegates never opens a port.
- **Server half** — when enabled, an inbound task runs `Run` in the background and streams the existing event bus back.
- **Client half** — a `remote_agent` tool dials the cluster, gets a task id immediately, and (per §9) the result wakes the caller on completion.
- **Protocol (example transport)** — `POST /delegate` (returns a task id), `GET /task/{id}` (poll fallback), `POST /callback` on the caller (push), with bearer-token auth and an allowlist.
- **Safety** — a depth/ancestry **cycle guard** (refuse if the caller's span is already an ancestor) prevents A→B→A loops; inbound tasks route through the same permission veto as §13.2.

**Where the code would live.** IKC is intended as a self-contained `ikc/` package: the transport (client + server), auth middleware, and the discovery client. The kernel itself gains only a thin seam — an opt-in `Serve` entry and a delegator the `remote_agent` tool calls — so the core loop stays transport-agnostic and the cluster/discovery details never leak into kernel code or the agent's prompt.

### 13.2 Permission gating

[`EventPermissionRequest`](../events.go#L11) and its `PermissionPayload` (with a `Verdict` field of `allow` / `deny`) exist but are never fired. To make them real:

- Fire `EventPermissionRequest` **synchronously, before** a tool is dispatched, from a single central point that wraps *all* registry tools (so built-ins, future MCP tools, and IKC delegation are all gated uniformly).
- The hook chain is already synchronous and can abort on error — extend it so a hook can set the `Verdict`; a `deny` short-circuits execution and returns a structured failure to the model instead of running the tool.
- Layer policy on top: per-tool allow / ask / deny lists and modes, so a host can auto-allow reads, prompt on writes, and deny shell — the host owns the UI, the kernel owns enforcement.

**How the policy would be configured.** Two layers, so it works both declaratively and programmatically:

- **Declarative default** — a `PermissionPolicy` in `Config` (loadable from a config file): ordered rules of `{ tool glob, mode: allow | ask | deny }`, e.g. allow `read/glob/grep`, ask on `write/edit`, deny `bash`. First matching rule wins; default-deny if nothing matches.
- **Programmatic override** — the host registers an `ask` callback (the `EventPermissionRequest` hook) that sets the `Verdict` for anything the static rules route to `ask`; this is where the host's UI / prompt lives. The kernel enforces the returned verdict.

This is the SDK's most load-bearing missing contract: an embeddable kernel must let the host veto side effects, and it is a prerequisite for opening the kernel to remote IKC delegation (§13.1) — a remote task is the highest-trust surface there is.

---

## Caveats and known gaps

- **Roadmap is not implemented.** §13 (IKC and permission gating) is designed but not code: there is no `ikc/` package and `EventPermissionRequest` is never fired. Treat §13 as direction, not capability.
- **String-sniffed tool errors.** Tool failure is detected by the `"Error:"` text prefix in [`OnToolResult`](../kernel.go#L765) rather than a structured error flag, so a legitimate result that begins with "Error:" would be misclassified as a failure.
- **Panics on history-validation failure.** `Stream` `panic()`s if the system message is not first or the last message is not a user message ([kernel.go#L535](../kernel.go#L535), [kernel.go#L539](../kernel.go#L539)) — a hard failure rather than a returned error.
- **Compaction relabels a user message as assistant** ([kernel.go#L933](../kernel.go#L933)), which some stricter provider APIs may reject.
- **Shared SQLite handle is process-global** and never explicitly closed; `Close()` only checkpoints the WAL. The handle is released on process exit.
- **Line numbers** in this document reflect the code at the time of writing (2026-06-23) and will drift as the source changes.

## Source files

- [`kernel.go`](../kernel.go) — construction, turn loop, compaction, subagents, background agents, hooks
- [`events.go`](../events.go) — event kinds, payloads, and the canonical `OTEL()` projection
- [`store.go`](../store.go) — single SQLite store, schema, trace/span graph, `OTELSpans`
- [`otlp.go`](../otlp.go) — OTLP/HTTP-JSON exporter and `LangfuseOTLP`
- [`provider.go`](../provider.go) — provider resolution + gateway transport (trace headers, cost-header capture)
- [`multimodal.go`](../multimodal.go) — inline-image handling in user prompts (size cap + vision-capability gate)
- [`utils.go`](../utils.go) — Snowflake IDs and OTEL ID derivation
- [`usage.go`](../usage.go) — `Usage`: tokens + gateway-reported cost (`PricingOK` honesty)
- [`model.go`](../model.go) — model catalog (`ResolveModel`): cost + context window + modalities
- [`step.go`](../step.go) — the Step interface (one llm-step): `Complete` / `Stream` / `CompleteObject`
- [`gatewaystep.go`](../gatewaystep.go) — default Step backend over the in-repo `llm.Client`
- [`fauxstep.go`](../fauxstep.go) — in-memory, network-free Step for tests
- [`steploop.go`](../steploop.go) — opt-in kernel-owned tool loop over the Step layer
- [`gateway.go`](../gateway.go) — gateway env vars + per-chat trace context
- [`handoff.go`](../handoff.go) — cross-model handoff transform (`TransformForHandoff`)
- [`history.go`](../history.go) — history reconstruction helpers
- [`tools/`](../tools) — built-in tool implementations
- [`examples/`](../examples) — runnable usage patterns
