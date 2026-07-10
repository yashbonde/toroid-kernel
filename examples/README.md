# toroid-kernel examples

Each subdirectory is a standalone, runnable program demonstrating one usage
pattern. They are intentionally small and heavily commented — if you are an AI
agent or a new contributor, read these to learn the API surface and the
canonical way to use each feature.

Run any example from the repo root:

```bash
export LLM_GATEWAY_BASE_URL=https://my-gateway.example.com/v1
export LLM_GATEWAY_KEY=sk-...           # default model is llmgateway/claude-haiku-4-5
go run ./examples/<name>
```

## Pattern index

| Example      | Feature                                   | Key APIs |
|--------------|-------------------------------------------|----------|
| `running`    | Blocking run + streaming run              | `NewKernel`, `Run`, `Stream`, `On(EventToken)`, `RunningCostUSD`, `Close` |
| `delegation` | Subagents, background agents, OTEL export | `subagent`/`subagent_async` tools, `RunSubagent`, `SpawnBackground`, `On(EventSubagentStart/TaskCompleted/MasterIdle)`, `Save`, `OTELSpans`, `ListSessions` |
| `events`     | Lifecycle observability + notify sinks    | `On(EventPreToolUse/PostToolUse/TurnCost/Notification)`, `tools.RegisterNotifySink` |
| `toroid-cli` | CLI runner that emits every event as NDJSON on stdout — wrap it in a subprocess from any language | `NewKernel`, `OnAll`, `Run`, JSON-encoded `Event` |
| `repl` | Interactive, pretty-printing chat REPL — rendered Markdown answers, trimmed tool-call lines, running cost | `NewKernel`, `On(EventPreToolUse/PostToolUse/Reasoning)`, `Run`, `RunningCostUSD` |
| `usage-with-mcp` | Connect to a remote MCP server (Slack's hosted server) and let the model call its tools alongside the built-ins | `Config.MCPServers`, `tools.MCPServerConfig`, `tools.ConnectMCPServer` |

`toroid-cli` takes the prompt as an argument and is the recommended way to embed
the kernel in a non-Go host:

```bash
go run ./examples/toroid-cli 'what files are in this directory?'
# each stdout line is one JSON-encoded toroid.Event; diagnostics go to stderr
```

`repl` is the human-facing counterpart: an interactive loop that renders
Markdown answers (headings, **bold**, `code`, fenced blocks, lists), shows tool
calls as compact trimmed one-liners, and tracks running cost. Targeting knobs are
environment variables; per-run toggles are flags:

```bash
export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
TOROID_MODEL=llmgateway/claude-haiku-4-5 go run ./examples/repl --thinking high --save
# in-REPL: /help /cost /model /reset /clear /exit (or Ctrl-D)
```

| Env var | Meaning | Default |
|---------|---------|---------|
| `TOROID_MODEL` | model id | `llmgateway/claude-haiku-4-5` |
| `TOROID_LLM_TOKEN` | API key for the provider | _(required)_ |
| `TOROID_MAX_ITER` | max tool iterations | kernel default (100) |
| `TOROID_TRIM` | max chars per tool arg/result line | 120 |

| Flag | Meaning | Default |
|------|---------|---------|
| `--save` | persist events/costs to the SQLite store | off |
| `--thinking` | `none` \| `low` \| `high` | `low` |
| `--no-colour` | disable ANSI styling | off |

## Core concepts

- **Construct once, reuse.** `toroid.NewKernel(ctx, toroid.Config{...})` wires the
  gateway client, tools, event bus, and (when `Save: true`) the SQLite store. Always
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
  `~/.toroid/sql.db`; `toroid.OTELSpans(traceID)` exports spec-valid
  OpenTelemetry spans for any OTLP backend.

## Models

Everything runs through the LiteLLM gateway (`LLM_GATEWAY_BASE_URL` +
`LLM_GATEWAY_KEY`). Model ids are `llmgateway/<name>` where `<name>` is
whatever the gateway routes — `claude-haiku-4-5`, `gpt-5.4-mini`, `kimi-k2p6`,
`glm-5p1`, `minimax-m2p7`, …. See the repo root `README.md`.
