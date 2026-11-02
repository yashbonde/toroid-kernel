# Toroid

Toroid is a Go kernel for agents. The agents use tools, keep context, delegate
work, report cost, and write a permanent execution trace.

Toroid owns the agent loop. You give a model and a working directory. Toroid
does the model calls, the tool calls, the context compaction, the retries, the
events, the storage, and the spend limits. The model wire is in this repository.
It does not use a provider SDK.

```text
your program
    │
    ▼
Toroid Kernel ── tools ── files, shell, skills, MCP, subagents
    │
    ▼
one LLM step ── LiteLLM gateway or OpenAI-compatible (OpenAI direct)
    │
    └────────── events, usage, SQLite, OpenTelemetry
```

## Why Toroid

- One kernel API for blocking runs, streaming, tools, structured output, and
  background agents.
- A stable system prompt and tool list for provider prompt caches.
- The real cost of each call from the gateway, not a preset. Direct OpenAI
  routes use fallback rates.
- Hard limits for each turn and for the full transcript.
- Built-in file and shell tools, lazy skills, remote MCP tools, and synchronous
  or background subagents.
- Automatic context compaction and repeat-call protection for long tool loops.
- Images and PDFs from Markdown paths or tool results. The model capability
  controls this.
- An event stream and an always-on NDJSON transcript.
- Optional SQLite storage and OpenTelemetry export.

## Requirements

- Go 1.26.4 or later.
- One provider key.
- The variable `LLM_GATEWAY_BASE_URL` for a LiteLLM gateway.

## Try the CLI

Build the example CLI as `trk`.

```bash
git clone https://github.com/yashbonde/toroid-kernel.git
cd toroid-kernel
go build -o trk ./examples/cli
```

Select one provider.

```bash
# LiteLLM gateway
export LLM_GATEWAY_BASE_URL=https://gateway.example.com/v1
export LLM_GATEWAY_KEY=your_gateway_key
./trk models
./trk --model llmgateway/claude-haiku-4-5

# OpenAI direct
export OPENAI_API_KEY=your_openai_key
./trk --model openai/gpt-5.4-mini
```

The terminal UI shows Markdown. The composer stays at the bottom. The UI shows
the tool activity and the context use while the agent works.

| Action | Key |
|---|---|
| Send | `Enter` |
| Insert a line | `Shift+Enter` |
| Scroll | `Page Up` / `Page Down` or `Ctrl+↑` / `Ctrl+↓` |
| Cancel the active turn | `Esc` |
| Quit | `Ctrl+C` |

On macOS, Page Up and Page Down are usually `Fn+↑` and `Fn+↓`.

Run one prompt without the TUI.

```bash
./trk --model openai/gpt-5.4-mini \
  --run 'Summarize this repository and identify release blockers.' \
  --plain
```

Without `--plain`, one-shot mode writes the kernel events as NDJSON to stdout.
This is the simplest integration for a host in another language.

## Use Toroid from Go

Install the module.

```bash
go get github.com/yashbonde/toroid-kernel
```

Create one kernel. Reuse it for the conversation.

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

`Run` returns the final answer. `Stream` drives the same tool loop and writes the
final response to an `io.Writer`.

```go
err := kernel.Stream(ctx, "Explain the changes as you inspect them.", os.Stdout)
```

## Observe the agent

Events are the host integration interface. Hooks run in order. A hook can stop
the event chain when it returns an error.

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

Useful events include the tool start and completion, the reasoning deltas, the
turn cost, the compaction, the subagent lifecycle, the background-task
completion, and the idle state. See [`events.go`](events.go) for the full event
list.

Each session also writes the events to:

```text
~/.toroid/sessions/<session-id>/transcript.jsonl
```

This transcript does not depend on SQLite storage.

## Add a tool

Register typed host functions with the built-in tools.

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

Set `IncludeComputerTools` to enable the core toolset: `read`, `write`, `edit`,
`multiedit`, and `bash`. The bash commands are non-interactive. They have a
timeout. They kill their process group when the caller cancels them. Large tool
results go to the session directory. Toroid does not discard them.

## Return structured data

`WithSchema` runs the normal tool loop. Then it forces the model to return a JSON
object that agrees with the supplied schema.

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

The final structured step is a billed model step. It obeys the same spend limits
as the rest of the run.

## Control spend

Set a limit for the full transcript in the kernel config. You can also set a
limit for each call.

```go
kernel, err := toroid.NewKernel(ctx, toroid.Config{
	Model:                 "llmgateway/claude-haiku-4-5",
	MaxTranscriptSpendUSD: 2.00,
})

answer, usage, err := kernel.Run(ctx, prompt,
	toroid.WithMaxTurnSpendUSD(0.25),
)
```

A provider reports the cost only after it completes a response. Thus Toroid bills
the step that crosses a limit. Toroid stops every later model step in that call.
This includes the structured-output steps and the background wake steps.

`Usage.PricingOK` separates a known zero from unknown pricing. Toroid never
reports an unpriced response as free.

## Providers and model IDs

The prefix selects the wire and the default credential source.

| Model ID | Wire | Credential | Cost source |
|---|---|---|---|
| `llmgateway/<model>` | OpenAI-compatible chat completions through LiteLLM | `LLM_GATEWAY_KEY` | LiteLLM response-cost header when present |
| `openai/<model>` | OpenAI API | `OPENAI_API_KEY` | Cached family rates |

Set `Config.APIKey` to replace the environment credential. The gateway routes
also need `LLM_GATEWAY_BASE_URL`, with its `/v1` part.

At startup, Toroid asks the gateway for the context limit and the output limit of
the selected model. If the gateway does not answer, the local model-family
catalog gives conservative values. Unknown models stay text-only and unpriced
until the host gives better information.

The OpenAI-compatible routes use the automatic prefix cache of their provider.

## Configuration reference

`NewKernel` expands the zero values where noted.

| Field | Purpose | Default behavior |
|---|---|---|
| `Model` | Provider and model ID | Required for useful work |
| `APIKey` | Explicit provider credential | From the provider environment variable |
| `WorkDir` | Tool working directory | A session directory below the current directory |
| `MaxIter` | Maximum model/tool steps in one turn | `100` |
| `MaxTokens` | Maximum output tokens for one model step | Provider default |
| `MaxRepeatCalls` | Consecutive equal call/result pairs before a stop | Disabled at `0`; set `3` for the recommended guard |
| `Thinking` | Reasoning budget: `none`, `low`, or `high` | `none` in the kernel; `low` in the CLI |
| `IncludeComputerTools` | Register the file and shell tools | Opt-in boolean |
| `IncludeSubagentTools` | Register `subagent` and `subagent_async` | Opt-in boolean |
| `Tools` | Host tool registry, merged at startup | None |
| `LoadSkills` | Find the skills in `~/.toroid/skills` and `~/.agents/skills` | Enabled when unset |
| `MCPServers` | Remote MCP servers for startup registration | None |
| `Save` | Store the trace data in SQLite | `false` |
| `Resume` | Rebuild the stored session history | `false` |
| `TotalContextSize` | Effective context window | Gateway limit or `200000` |
| `CompactionBufferSize` | Reserved tokens before the automatic compaction | `50000` |
| `SmallerModel` | Model for the compaction and the subagents | `deepseek-v4-flash` |
| `MaxTranscriptSpendUSD` | Limit for the full kernel spend | Disabled when not positive |

For each exported type and method, use the Go package reference.

```bash
go doc -all github.com/yashbonde/toroid-kernel
```

## Skills, MCP, and delegation

Skills load on demand. At startup Toroid reads only the `name` and the
`description`. Toroid finds them in two places:

- `~/.toroid/skills/*.md`
- `~/.agents/skills/<name>/SKILL.md`

The model loads the full body of a skill through the `skill` tool only when it is
necessary. When the same skill name is in both places, the `~/.toroid/skills`
copy wins. Set `LoadSkills` to `false` to stop the discovery.

Toroid connects the remote MCP servers from `Config.MCPServers` during the kernel
construction. Toroid adds a prefix to their tools. Then it merges the tools with
the core registry and the host registry.

Set `IncludeSubagentTools: true` to expose these tools:

- `subagent`, which does synchronous delegated work.
- `subagent_async`, which returns immediately. It wakes an idle kernel when the
  background task finishes.

Programmatic hosts can use `RunSubagent` and `SpawnBackground` directly. The
child kernels inherit the root trace ID. Thus the delegated work stays in one
observable trace tree.

## Persistence and telemetry

With `Save: true`, Toroid stores the trace metadata, the spans, the events, and
the costs in:

```text
~/.toroid/sql.db
```

Each kernel is one span. A root kernel has `TraceID == SessionID`. The subagents
share that trace ID and identify their parent span. `OTELSpans(traceID)` maps the
stored tree to OpenTelemetry spans. `LangfuseOTLP` sends a stored trace to a
Langfuse OTLP endpoint.

Always call `Close()`. It checkpoints the SQLite write-ahead log.

## How the loop works

`Kernel.Run` and `Kernel.Stream` drive the same loop:

1. Compile a cache-stable system prefix. The tools, the skills, the MCP servers,
   and the subagent capabilities are known at this point.
2. Add the user message. Resolve the supported Markdown media paths.
3. Ask the selected `Step` for one model response.
4. Run the requested tools. Append their results.
5. Repeat until the model answers, a guard trips, Toroid compacts the context,
   the caller cancels, or a spend limit is reached.
6. Emit the final events, the usage, and the stored trace data.

The `Step` interface is exactly one model request. Production uses `GatewayStep`.
Tests can replace it with `FauxStep` for a deterministic tool loop without a
network.

Read the [architecture guide](assets/ARCHITECTURE.md) for the state machine, the
event order, the context management, the persistence schema, and the delegation
model.

## Examples

Start with [`examples/running`](examples/running/main.go). It shows blocking and
streaming runs, events, custom tools, guardrails, delegation, multimodal input,
structured output, and OTEL export in one program.

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

See the [examples index](examples/README.md) for the CLI, the hosted MCP, the
Langfuse, and the observability examples.

## Development

```bash
go test ./...
go vet ./...
go build ./examples/cli
```

The project uses a pure-Go SQLite driver. It does not need CGO.

## License

[MIT](LICENSE)
