# toroid-kernel

`toroid-kernel` is a Go package for running tool-using LLM agents with persistent traces, resumable history, model-cost accounting, and a built-in tool registry.

## Features

- Agent kernel built on `charm.land/fantasy`
- Provider selection from `provider/model` IDs
- Session persistence with bbolt and SQLite-backed task state
- Conversation compaction and history reconstruction
- Built-in filesystem, shell, search, notification, and subagent tools

## Install

```bash
go get github.com/yashbonde/toroid-kernel
```

Requires Go `1.26.1` or newer.

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

	out, usage, err := kernel.Run(ctx, "Summarize the repository and point out release blockers.")
	if err != nil {
		panic(err)
	}

	fmt.Println(out)
	fmt.Printf("usage sessions: %d\n", len(usage.Tokens))
}
```

## Run The Example

Set `GEMINI_TOKEN`, then run the example program from the repository root.

```bash
export GEMINI_TOKEN=your_api_key
go run ./examples -prompt 'Reply with the word OK and nothing else.'
```

You can also use the built-in demo flows:

```bash
go run ./examples -sequence
go run ./examples -block
```

## Provider Examples

`toroid` currently supports `google`, `anthropic`, and `openai` model prefixes.

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

Each provider uses the same `toroid.Config` shape. The only required changes are the `Model` prefix and the API key you pass in.

## Release Notes

- The module path is `github.com/yashbonde/toroid-kernel`.
- Embedded prompts live in `prompts/` and pricing assets live in `assets/`.
- Runtime state is stored under `~/.swarmbuddy/`.

## Status

This repository currently ships as a library package. A license file is not included yet, so choose and add one before publishing a public release.
