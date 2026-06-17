# toroid-kernel examples

Each subdirectory is a standalone, runnable program demonstrating one usage
pattern. They are intentionally small and heavily commented — if you are an AI
agent or a new contributor, read these to learn the API surface and the
canonical way to use each feature.

Run any example from the repo root:

```bash
export ANTHROPIC_API_KEY=your_api_key   # default model is anthropic/claude-haiku-4-5; see "Providers" below
go run ./examples/<name>
```

## Pattern index

| Example      | Feature                                   | Key APIs |
|--------------|-------------------------------------------|----------|
| `running`    | Blocking run + streaming run              | `NewKernel`, `Run`, `Stream`, `On(EventToken)`, `RunningCostUSD`, `Close` |
| `delegation` | Subagents, background agents, OTEL export | `subagent`/`subagent_async` tools, `RunSubagent`, `SpawnBackground`, `On(EventSubagentStart/TaskCompleted/MasterIdle)`, `Save`, `OTELSpans`, `ListSessions` |
| `events`     | Lifecycle observability + notify sinks    | `On(EventPreToolUse/PostToolUse/TurnCost/Notification)`, `tools.RegisterNotifySink` |

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

Every example uses `anthropic/claude-haiku-4-5` (the kernel default), but the
only change needed for another provider is the `Model` prefix and the API key:

| Prefix       | Model env / key            |
|--------------|----------------------------|
| `google`     | `GEMINI_TOKEN`             |
| `anthropic`  | `ANTHROPIC_API_KEY`        |
| `openai`     | `OPENAI_API_KEY`           |
| `llmgateway` | `LLM_GATEWAY_BASE_URL` + bearer key |

See the repo root `README.md` for provider config snippets.
