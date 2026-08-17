# Toroid

Toroid is a Go kernel for building agents that can use tools, retain context,
delegate work, report their real cost, and leave a durable execution trace.

It owns the agent loop. You provide a model and a working directory; Toroid
handles model calls, tool execution, context compaction, retries, events,
persistence, and spend limits. The model wire is implemented in this repository,
without a provider SDK.

```text
your program
    │
    ▼
Toroid Kernel ── tools ── files, shell, skills, MCP, subagents
    │
    ▼
one LLM step ── LiteLLM gateway, OpenAI, or Anthropic
    │
    └────────── events, usage, SQLite, OpenTelemetry
```

## Why Toroid

- One kernel API for blocking runs, streaming, tools, structured output, and
  background agents.
- A stable system-prompt and tool prefix designed for provider prompt caches.
- Real per-call cost from LiteLLM when available, with explicit fallback rates
  for direct OpenAI and Anthropic routes.
- Hard turn and transcript spend limits.
- Built-in file and shell tools, lazy-loaded skills, remote MCP tools, and
  optional synchronous or background subagents.
- Automatic context compaction and repeat-call protection for long tool loops.
- Images and PDFs from Markdown paths or tool results, gated by model capability.
- An observable event stream plus an always-on NDJSON transcript.
- Optional SQLite persistence and OpenTelemetry export.

## Requirements

- Go 1.26.4 or newer
- A supported provider key
- `LLM_GATEWAY_BASE_URL` when using a LiteLLM gateway

## Try the CLI

Build the example CLI as `trk`:

```bash
git clone https://github.com/yashbonde/toroid-kernel.git
cd toroid-kernel
go build -o trk ./examples/cli
```

Choose one provider:

```bash
# LiteLLM gateway
export LLM_GATEWAY_BASE_URL=https://gateway.example.com/v1
export LLM_GATEWAY_KEY=your_gateway_key
./trk models
./trk --model llmgateway/claude-haiku-4-5

# OpenAI direct
export OPENAI_API_KEY=your_openai_key
./trk --model openai/gpt-5.4-mini

# Anthropic direct
export ANTHROPIC_API_KEY=your_anthropic_key
./trk --model anthropic/claude-haiku-4-5
```

The terminal UI renders Markdown, keeps the composer pinned to the bottom, and
shows tool activity and context usage as the agent works.

| Action | Key |
|---|---|
| Send | `Enter` |
| Insert a line | `Shift+Enter` |
| Scroll | `Page Up` / `Page Down` or `Ctrl+↑` / `Ctrl+↓` |
| Cancel the active turn | `Esc` |
| Quit | `Ctrl+C` |

On macOS, Page Up and Page Down are usually `Fn+↑` and `Fn+↓`.

Run one prompt without opening the TUI:

```bash
./trk --model openai/gpt-5.4-mini \
  --run 'Summarize this repository and identify release blockers.' \
  --plain
```

Without `--plain`, one-shot mode writes kernel events as NDJSON to stdout. This
is the simplest integration path for a host written in another language.

## Use Toroid from Go

Install the module:

```bash
go get github.com/yashbonde/toroid-kernel
```

Create one kernel and reuse it for the conversation:

```go
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	ctx := context.Background()

	kernel, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                "openai/gpt-5.4-mini",
		APIKey:               os.Getenv("OPENAI_API_KEY"),
		WorkDir:              ".",
		IncludeComputerTools: true,
		Save:                 true,
	})
	if err != nil {
		panic(err)
	}
	defer kernel.Close()

	answer, usage, err := kernel.Run(ctx,
		"Inspect this repository and summarize its architecture.")
	if err != nil {
		panic(err)
	}

	fmt.Println(answer)
	fmt.Printf("sessions: %d, cost: $%.6f\n",
		len(usage.Tokens), kernel.RunningCostUSD())
}
```

`Run` returns the completed answer. `Stream` drives the same tool loop and
writes the final response to an `io.Writer`:

```go
err := kernel.Stream(ctx, "Explain the changes as you inspect them.", os.Stdout)
```

## Observe the agent

Events are the host integration surface. Hooks run synchronously and may abort
the event chain by returning an error.

```go
kernel.On(toroid.EventPreToolUse, func(ctx context.Context, event toroid.Event) error {
	payload, ok := event.Payload.(*toroid.ToolUsePayload)
	if ok {
		fmt.Printf("tool: %s %s\n", payload.Name, payload.Args)
	}
	return nil
})

kernel.On(toroid.EventTurnCost, func(ctx context.Context, event toroid.Event) error {
	payload, ok := event.Payload.(*toroid.TurnCostPayload)
	if ok {
		fmt.Printf("turn=$%.6f total=$%.6f\n",
			payload.TurnCostUSD, payload.TotalCostUSD)
	}
	return nil
})
```

Useful events include tool start and completion, reasoning deltas, turn cost,
compaction, subagent lifecycle, background-task completion, and idle state. See
[`events.go`](events.go) for the full event vocabulary.

Every session also writes observable events to:

```text
~/.toroid/sessions/<session-id>/transcript.jsonl
```

This transcript exists independently of SQLite persistence.

## Add a tool

Register typed host functions alongside Toroid's built-in tools:

```go
type SearchArgs struct {
	Query string `json:"query"`
}

kernel.Tools.Register(&tools.ToolDef{
	Name:        "search_docs",
	Description: "Search the product documentation",
	Handler: llm.NewTool(
		"search_docs",
		"Search the product documentation",
		func(ctx context.Context, args SearchArgs) (llm.ToolResult, error) {
			return llm.NewTextResult(search(args.Query)), nil
		},
	),
})
```

The core toolset contains `read`, `write`, `edit`, `multiedit`, and `bash` when
`IncludeComputerTools` is enabled. Bash commands are non-interactive, have a
timeout, and kill their process group when cancelled. Large tool results spill
to the session directory instead of being silently discarded.

## Return structured data

`WithSchema` runs the normal tool loop, then forces the model to return a JSON
object matching the supplied schema:

```go
type Review struct {
	Summary  string   `json:"summary"`
	Blockers []string `json:"blockers"`
}

schema := toroid.GenerateSchema(reflect.TypeOf(Review{}))
answer, _, err := kernel.Run(ctx, "Review this repository.",
	toroid.WithSchema(schema, "review", "Repository review result"),
)
```

The final structured pass is a billed model step and obeys the same spend
limits as the rest of the run.

## Control spend

Set a cumulative limit in the kernel config and an optional per-call limit on
individual runs:

```go
kernel, err := toroid.NewKernel(ctx, toroid.Config{
	Model:                 "llmgateway/claude-haiku-4-5",
	MaxTranscriptSpendUSD: 2.00,
})

answer, usage, err := kernel.Run(ctx, prompt,
	toroid.WithMaxTurnSpendUSD(0.25),
)
```

A provider reports cost only after completing a response, so the step that
crosses a limit is billed. Toroid prevents every later model step in that call,
including structured-output and background wake steps.

`Usage.PricingOK` distinguishes a known zero from unknown pricing. Toroid never
pretends an unpriced response was free.

## Providers and model IDs

The prefix selects the wire and default credential source:

| Model ID | Wire | Credential | Cost source |
|---|---|---|---|
| `llmgateway/<model>` | OpenAI-compatible chat completions through LiteLLM | `LLM_GATEWAY_KEY` | LiteLLM response-cost header when present |
| `openai/<model>` | OpenAI API | `OPENAI_API_KEY` | Cached family rates |
| `anthropic/<model>` | Native Anthropic Messages API | `ANTHROPIC_API_KEY` | Cached family rates |

Pass `Config.APIKey` to override the environment credential. Gateway routes
also require `LLM_GATEWAY_BASE_URL`, including its `/v1` segment.

At startup, Toroid asks a configured gateway for the selected model's context
and output limits. If the gateway cannot answer, the local model-family catalog
provides conservative capabilities. Unknown models remain text-only and
honestly unpriced until the host supplies better information.

Anthropic's direct route adds explicit cache-control breakpoints to the stable
system prefix and recent conversation. OpenAI-compatible routes rely on their
provider's automatic prefix caching.

## Configuration reference

Zero values are expanded by `NewKernel` where noted.

| Field | Purpose | Default behavior |
|---|---|---|
| `Model` | Provider and model ID | Required for useful execution |
| `APIKey` | Explicit provider credential | Resolved from the provider environment variable |
| `WorkDir` | Tool working directory | A per-session directory below the current directory |
| `MaxIter` | Maximum model/tool steps in one turn | `100` |
| `MaxTokens` | Maximum output tokens for one model step | Provider default |
| `MaxRepeatCalls` | Consecutive identical call/result pairs before stopping | Disabled at `0`; set `3` for the recommended guard |
| `Thinking` | `none`, `low`, or `high` reasoning budget | `none` in the kernel; `low` in the CLI |
| `IncludeComputerTools` | Register file and shell tools | Opt-in boolean |
| `IncludeSubagentTools` | Register `subagent` and `subagent_async` | Opt-in boolean |
| `Tools` | Host tool registry merged at startup | None |
| `LoadSkills` | Discover `~/.toroid/skills/*.md` | Enabled when unset |
| `MCPServers` | Remote MCP servers whose tools are registered at startup | None |
| `Save` | Persist trace data to SQLite | `false` |
| `Resume` | Reconstruct stored session history | `false` |
| `TotalContextSize` | Effective context window | Gateway limit or `200000` |
| `CompactionBufferSize` | Reserved tokens before automatic compaction | `50000` |
| `SmallerModel` | Model for compaction and subagents | `deepseek/deepseek-v4-flash-0731` |
| `MaxTranscriptSpendUSD` | Cumulative kernel spend limit | Disabled when non-positive |

For every exported type and method, use Go's package reference:

```bash
go doc -all github.com/yashbonde/toroid-kernel
```

## Skills, MCP, and delegation

Skills use progressive disclosure. At startup Toroid reads only `name` and
`description` from `~/.toroid/skills/*.md`; the model loads a full skill body
through the `skill` tool only when needed. Set `LoadSkills` to `false` to turn
discovery off.

Remote MCP servers configured through `Config.MCPServers` are connected during
kernel construction. Their discovered tools are namespaced and merged with the
core and host tool registries.

Set `IncludeSubagentTools: true` to expose:

- `subagent`, which performs synchronous delegated work.
- `subagent_async`, which returns immediately and wakes an idle kernel when the
  background task finishes.

Programmatic hosts can use `RunSubagent` and `SpawnBackground` directly. Child
kernels inherit the root trace ID, so delegated work remains in one observable
trace tree.

## Persistence and telemetry

With `Save: true`, Toroid stores trace metadata, spans, events, and costs in:

```text
~/.toroid/sql.db
```

Each kernel is one span. A root kernel has `TraceID == SessionID`; subagents
share that trace ID and identify their parent span. `OTELSpans(traceID)` maps the
stored tree to OpenTelemetry spans, and `LangfuseOTLP` sends a stored trace to a
Langfuse OTLP endpoint.

Always call `Close()` so the SQLite write-ahead log is checkpointed.

## How the loop works

`Kernel.Run` and `Kernel.Stream` drive the same loop:

1. Compile a cache-stable system prefix after tools, skills, MCP servers, and
   subagent capabilities are known.
2. Add the user message, resolving supported Markdown media paths.
3. Ask the selected `Step` for one model response.
4. Execute requested tools and append their results.
5. Repeat until the model answers, a guard trips, context is compacted, the
   caller cancels, or a spend limit is reached.
6. Emit the final events, usage, and persisted trace data.

The `Step` interface represents exactly one model request. Production uses
`GatewayStep`; tests can replace it with `FauxStep` for deterministic,
network-free tool loops.

Read the [architecture guide](assets/ARCHITECTURE.md) for the state machine,
event ordering, context management, persistence schema, and delegation model.

## Examples

Start with [`examples/running`](examples/running/main.go). It demonstrates
blocking and streaming runs, events, custom tools, guardrails, delegation,
multimodal input, structured output, and OTEL export in one program.

```bash
# Full live tour
export LLM_GATEWAY_BASE_URL=https://gateway.example.com/v1
export LLM_GATEWAY_KEY=your_gateway_key
go run ./examples/running

# Network-free guardrail demonstration
go run ./examples/running --guardrails

# Offline integration suite for skills, MCP, tools, and cache stability
go test ./examples/e2e-test
```

See the [examples index](examples/README.md) for the CLI, hosted MCP, Langfuse,
and observability examples.

## Development

```bash
go test ./...
go vet ./...
go build ./examples/cli
```

The project uses a pure-Go SQLite driver and does not require CGO.

## Project map

| Path | Responsibility |
|---|---|
| `kernel.go` | Kernel construction, runs, streaming, context, and background work |
| `steploop.go` | Kernel-owned model/tool loop |
| `step.go`, `gatewaystep.go`, `fauxstep.go` | One-request model abstraction and implementations |
| `llm/` | OpenAI-compatible and native Anthropic wires |
| `tools/` | Core tools, registry, skills, MCP, and subagent tool |
| `prompt_compiler.go` | Stable system-prompt and tool-contract compilation |
| `events.go`, `transcript.go` | Observable lifecycle and NDJSON transcript |
| `store.go`, `otlp.go` | SQLite persistence and OpenTelemetry projection |
| `examples/` | Runnable integrations and offline fixtures |

## License

[MIT](LICENSE)
