# one-shot-runner

The smallest possible toroid driver: one prompt in, one run, exit. It is
`examples/cli` stripped of everything interactive — no REPL, no TUI viewport,
no `/commands`, no delegate, no `models`/`sessions` subcommands, no raw-terminal
input handling.

Two output modes:

- **default** — every kernel event as NDJSON on stdout (the machine bridge for
  hosts written in other languages; diagnostics go to stderr).
- **--plain** — only the final assistant response on stdout; tool activity is
  summarised to stderr.

## Usage

```bash
export LLM_GATEWAY_BASE_URL=https://openrouter.ai/api/v1
export LLM_GATEWAY_KEY=...

go run ./examples/one-shot-runner --model stealth/ox-alpha 'what files are here?'
go run ./examples/one-shot-runner --plain --model stealth/ox-alpha 'summarise this repo'
```

Flags: `--model --thinking --save/--no-save --plain --tokens --context-size
--compact-buffer --max-iter --max-repeat-calls --smaller-model --max-spend
--max-tokens`. The prompt is the single positional argument.

## Binary sizes

Smallest compiled size per binary, with and without `upx --best`
(`upx` skips Mach-O arm64 binaries, so the macOS rows are uncompressed).
Build commands:

```bash
# cli
go build -ldflags "-s -w" -trimpath -o cli ./examples/cli
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -trimpath -o cli ./examples/cli

# one-shot-runner
go build -ldflags "-s -w" -trimpath -o one-shot-runner ./examples/one-shot-runner
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -trimpath -o one-shot-runner ./examples/one-shot-runner
```

| Binary          | OS           | Compile command                                              | Size      | + upx     |
|-----------------|--------------|--------------------------------------------------------------|-----------|-----------|
| cli             | macOS (arm64)| `go build -ldflags "-s -w" -trimpath -o cli ./examples/cli`  | 20.7 MiB  | n/a¹      |
| cli             | Linux (amd64)| `GOOS=linux GOARCH=amd64 go build …`                          | 21.3 MiB  | 7.9 MiB   |
| one-shot-runner | macOS (arm64)| `go build -ldflags "-s -w" -trimpath -o runner ./examples/one-shot-runner` | 11.2 MiB | n/a¹ |
| one-shot-runner | Linux (amd64)| `GOOS=linux GOARCH=amd64 go build …`                          | 11.6 MiB  | 4.9 MiB   |

¹ `upx` does not support Mach-O arm64; the binary is left as built.
