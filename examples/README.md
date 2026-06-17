# toroid-kernel examples

Each subdirectory is a standalone, runnable program demonstrating one usage
pattern. They are intentionally small and heavily commented — if you are an AI
agent or a new contributor, read these to learn the API surface and the
canonical way to use each feature.

Run any example from the repo root:

```bash
export GEMINI_TOKEN=your_api_key   # any provider key works; see "Providers" below
go run ./examples/<name>
```

Only `otel` runs without an API key (it drives the store directly).

## Pattern index

| Example         | Feature                        | Key APIs |
|-----------------|--------------------------------|----------|
| `blocking`      | One-shot run, get final text   | `NewKernel`, `Run`, `RunningCostUSD`, `Close` |
| `streaming`     | Stream tokens to a writer/UI   | `Stream`, `On(EventToken)` |
| `subagent`      | Synchronous delegation         | `subagent` tool, `On(EventSubagentStart/Stop)`, `RunSubagent` |
| `background`    | Async delegation + wake-on-done| `SpawnBackground`, `Wake`, `On(EventTaskCompleted/MasterIdle)`, `subagent_async` tool |
| `events`        | Observe the lifecycle          | `On(EventPreToolUse/PostToolUse/TurnCost)` |
| `notifications` | Pluggable notification sinks   | `tools.RegisterNotifySink`, `On(EventNotification)` |
| `otel`          | Persistence + OpenTelemetry    | `NewStore`, `Save`, `OTELSpans`, `ListSessions`, `DeleteSession` |

## Core concepts

- **Construct once, reuse.** `toroid.NewKernel(ctx, toroid.Config{...})` wires the
  provider, tools, event bus, and (when `Save: true`) the SQLite store. Always
  `defer kernel.Close()`.
- **Run vs Stream.** `Run` blocks and returns the full text; `Stream` writes
  incrementally to an `io.Writer`. Both drive the same tool-calling loop.
- **Events are the integration surface.** `kernel.On(kind, fn)` observes
  everything: tokens, reasoning, tool calls, costs, subagents, compaction,
  notifications, idle/task-completion. Hooks are synchronous; returning an error
  aborts the chain.
- **Delegation has three rungs.** `subagent` (in-process, synchronous) →
  background agent (`SpawnBackground`/`subagent_async`, async, wakes an idle
  kernel on completion) → inter-kernel communication (planned). All share one
  trace via `TraceID`/`ParentSpanID`.
- **Persistence is OTEL-native.** With `Save: true`, everything lands in
  `~/.swarmbuddy/sql.db`; `toroid.OTELSpans(traceID)` exports spec-valid
  OpenTelemetry spans for any OTLP backend.

## Providers

Every example uses `google/gemini-3-flash-preview`, but the only change needed
for another provider is the `Model` prefix and the API key:

| Prefix       | Model env / key            |
|--------------|----------------------------|
| `google`     | `GEMINI_TOKEN`             |
| `anthropic`  | `ANTHROPIC_API_KEY`        |
| `openai`     | `OPENAI_API_KEY`           |
| `llmgateway` | `LLM_GATEWAY_BASE_URL` + bearer key |

See the repo root `README.md` for provider config snippets.
