# toroid-kernel

`toroid-kernel` is a Go package for running tool-using LLM agents with persistent traces, resumable history, model-cost accounting, and a built-in tool registry.

## Features

- Agent kernel built on `charm.land/fantasy`
- Provider selection from `provider/model` IDs
- Single-file SQLite persistence (traces, costs, events, todos) with OpenTelemetry-compatible trace/span IDs
- Conversation compaction and history reconstruction
- Built-in filesystem, shell, search, notification, and subagent tools
- Background agents — async subagents that wake an idle kernel on completion
- Platform-agnostic, pluggable notifications

## Architecture

For a deep dive into how the kernel is wired — construction, the turn loop,
tools, the single SQLite store, OTEL-native telemetry, context management,
sub/background agents, and the roadmap — see the
[architecture explainer](assets/ARCHITECTURE.md). It links directly to the
relevant source files.

## Install

```bash
go get github.com/yashbonde/toroid-kernel
```

Requires Go `1.26.4` or newer.

## Quick Start

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
		Model:   "google/gemini-3-flash-preview",
		APIKey:  os.Getenv("GEMINI_TOKEN"),
		WorkDir: ".",
		Save:    true,
	})
	if err != nil {
		panic(err)
	}
	defer kernel.Close()

	out, usage, err := kernel.Run(ctx, "Summarize the repository and point out release blockers.")
	if err != nil {
		panic(err)
	}

	fmt.Println(out)
	fmt.Printf("usage sessions: %d\n", len(usage.Tokens))
}
```

## Examples

The [`examples/`](examples/) directory has small, focused, runnable programs —
one per usage pattern (running blocking/streaming, delegation to subagents and
background agents with OpenTelemetry export, and events/notifications). Start with
[`examples/README.md`](examples/README.md), which indexes every pattern and the
APIs it uses.

> **If you are an AI agent**, read [`examples/README.md`](examples/README.md)
> and the per-directory `main.go` files first — they are the canonical,
> up-to-date reference for how to use this library.

```bash
export ANTHROPIC_API_KEY=your_api_key
go run ./examples/running      # blocking Run + token streaming
go run ./examples/delegation   # subagents, background agents, OpenTelemetry export
go run ./examples/events       # lifecycle observability + notification sinks
```

## Provider Examples

`toroid` currently supports `google`, `anthropic`, `openai`, and `llmgateway` model prefixes.

### Google

```go
kernel, err := toroid.NewKernel(ctx, toroid.Config{
	Model:   "google/gemini-3-flash-preview",
	APIKey:  os.Getenv("GEMINI_TOKEN"),
	WorkDir: ".",
})
```

### Anthropic

```go
kernel, err := toroid.NewKernel(ctx, toroid.Config{
	Model:   "anthropic/claude-sonnet-4-5",
	APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
	WorkDir: ".",
})
```

### OpenAI

```go
kernel, err := toroid.NewKernel(ctx, toroid.Config{
	Model:   "openai/gpt-5",
	APIKey:  os.Getenv("OPENAI_API_KEY"),
	WorkDir: ".",
})
```

### LLM Gateway

Use the `llmgateway` prefix to talk to any OpenAI-compatible gateway (for
example a self-hosted [LiteLLM](https://github.com/BerriAI/litellm) proxy that
fronts many model families behind a single bearer-authenticated endpoint). The
gateway speaks the OpenAI `/v1/chat/completions` protocol, including streaming
and tool calls, so the full agentic loop works unchanged.

Set `LLM_GATEWAY_BASE_URL` to the gateway's OpenAI-compatible base, including
the `/v1` segment. The model name after the prefix is whatever the gateway routes — e.g. `claude-sonnet-4-6`,
`gpt-5.4-mini`, `kimi-k2p6`.

```go
// export LLM_GATEWAY_BASE_URL=https://my-gateway.example.com/v1
kernel, err := toroid.NewKernel(ctx, toroid.Config{
	Model:   "llmgateway/claude-sonnet-4-6",
	APIKey:  os.Getenv("LLM_GATEWAY_KEY"),
	WorkDir: ".",
})
```

Each provider uses the same `toroid.Config` shape. The only required changes are the `Model` prefix and the API key you pass in.

## Persistence & telemetry

When `Save: true`, traces, spans, costs, events, and todos are written to a
single SQLite database at `~/.swarmbuddy/sql.db`. Span IDs are time-ordered
[Snowflake](https://en.wikipedia.org/wiki/Snowflake_ID) IDs and the
trace/span/parent graph maps directly onto OpenTelemetry — `toroid.OTELSpans(traceID)`
returns spec-valid OTEL-shaped spans you can feed to any OTLP exporter. Call
`kernel.Close()` when done to checkpoint the store.

## Background agents

`subagent_async` runs a subtask in the background and returns immediately. When
it finishes, its result is queued and the kernel is woken to process it — even
if `Run`/`Stream` had already returned. The `MasterIdle` and `TaskCompleted`
events let a host observe these transitions.

## Notifications

The `notify` tool fires a `Notification` event on the kernel's event bus and
delivers a best-effort desktop notification (macOS / Linux / Windows). Register
additional sinks (webhook, Slack, a peer kernel) with `tools.RegisterNotifySink`.

## Release Notes

- The module path is `github.com/yashbonde/toroid-kernel`.
- Embedded prompts live in `prompts/` and pricing assets live in `assets/`.
- Runtime state is stored under `~/.swarmbuddy/` (single SQLite DB at `sql.db`).

## Status

This repository currently ships as a library package. A license file is not included yet, so choose and add one before publishing a public release.
