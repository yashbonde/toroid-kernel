# Changelog

All notable changes to toroid-kernel are documented here. This project follows
[Semantic Versioning](https://semver.org/).

## v0.4.0

Adds two extensibility primitives — on-demand Skills and an MCP client — plus
a runtime-state rename, REPL UX improvements, and a more robust pricing lookup.

### Added

- **Skills.** When `Config.LoadSkills` is unset or `true` (default), the kernel
  scans `~/.toroid/skills/*.md` at startup and loads only each file's
  frontmatter (`name` + `description`) into the system prompt. A new `skill`
  tool loads a skill's full body on demand — by the model recognizing a
  relevant skill, or a user naming one directly — so token cost scales with
  skills actually used, not skills that exist on disk.
- **MCP client.** `Config.MCPServers []tools.MCPServerConfig` connects to
  remote Model Context Protocol servers at kernel startup, built on
  `github.com/mark3labs/mcp-go`. Each server's tools are discovered via
  `tools/list` and registered into the kernel's tool registry, prefixed
  `<server>__<tool>` to avoid collisions across servers. Connections are
  closed in `Kernel.Close()`. Transport tries streamable HTTP first, then
  falls back to legacy SSE if the server rejects the initialize request with
  a 4xx. `MCPServerConfig.Headers` lets callers attach auth headers (e.g. an
  OAuth bearer token) to every request.
- **`toroid-cli -plain` flag.** Prints only the final assistant response as
  plain text instead of the NDJSON event stream — useful when the CLI is
  embedded as a subprocess that just needs the answer text.
- **`examples/usage-with-mcp`** — end-to-end example connecting the kernel to
  Slack's hosted MCP server, showing `Config.MCPServers` + `MCPServerConfig`
  with OAuth bearer-token headers.
- **REPL ESC-to-cancel.** The terminal is put in cbreak mode during a turn so
  a single ESC press cancels the running request via `context.Cancel`. Falls
  back gracefully on platforms without termios support.
- **REPL reasoning rendering.** Thinking/reasoning output is now shown inside
  dimmed separator lines with proper newline handling, so tool calls and
  results don't bleed into reasoning text.
- **REPL turn footer.** Shows output-token throughput (`N tok/s`) alongside
  running cost. When the model isn't in `pricing.json` (e.g. gateway-routed),
  the footer reads "pricing unavailable" instead of a misleading `$0.000000`.
- **REPL workdir display.** The workdir is now resolved to an absolute path
  and displayed relative to `$HOME` (e.g. `~/projects/foo`) in the banner and
  `/model` command.
- `assets/benchmark.md` — benchmark selection (Terminal-Bench, Tau-bench,
  Harness-Bench methodology) and a feature-by-feature comparison against
  Claude Code, the Claude Agent SDK, OpenAI Codex CLI, and pi.dev. Updated
  to reflect that toroid now has partial MCP and Skills support, with revised
  radar chart and gap analysis.
- `assets/standard_pricing.md` — notes on gateway vs local pricing estimation
  (LiteLLM `x-litellm-response-cost` header behaviour for streaming vs
  non-streaming calls).

### Changed

- **Runtime state moved from `~/.swarmbuddy` to `~/.toroid`.** Affects the
  SQLite store path, the prompt/asset override directories, and the new
  skills directory. This is a breaking change for anyone relying on the old
  path — no migration is performed automatically.
- **Pricing lookup rewrite (`pricing.go`).** `GetModelPricing` now strips
  `llmgateway/` prefixes, expands version-dot normalization (covers more
  version patterns like `-4-8` → `-4.8`, `-5-5` → `-5.5`), and tries each
  name variant against bare keys plus `anthropic/`, `openai/`, and `google/`
  provider prefixes before falling back to the fuzzy prefix match. This
  resolves pricing for gateway-routed models that were previously missed.
- **REPL Markdown rendering.** Inline code now uses blue foreground instead
  of a background colour; fenced code blocks drop the `│` gutter prefix;
  strikethrough (`~~text~~`) is now rendered with ANSI SGR 9.

### Housekeeping

- `.gitignore` now covers Windows example binaries, the `usage-with-mcp`
  binary, and `*.err` scratch files.

## v0.3.3

This release consolidates three independent lines of work — a compaction
correctness fix, an agent loop guard, and structured-output support plus new
interactive examples — into a single cut.

### Fixed

- **Context compaction now tracks real window occupancy.** Auto-compaction was
  driven by `currentTokens`, which was set from `result.TotalUsage` — the *sum*
  of every step's usage across an agentic turn. With prompt caching, each step
  re-reads the whole (cached) context, so summing steps massively over-counted
  how full the window actually was, making compaction fire erratically and far
  from the true limit.
  - Added `windowTokens()` and based `currentTokens` on the **last step's**
    request size (`input + cache_read + cache_write + output`) instead of the
    summed `TotalUsage`. This is the actual occupancy of the context window.
  - Added `overContextThreshold()`, now checked at **every step boundary** inside
    `streamCurrent` (not only between `Run` calls). A long, tool-heavy turn that
    balloons the window mid-loop now compacts in place instead of overflowing.
  - `Compact()` now resets `currentTokens` to an estimate of the new (tiny)
    summary history, so the stale pre-compaction value can no longer immediately
    re-trigger compaction or misreport usage.

### Added

- **Loop guard (`Config.MaxRepeatCalls`, default 3).** The agent halts when the
  last N steps all issue the exact same tool calls *and* receive the exact same
  results, wired as a Fantasy stop condition so it stops cleanly at a step
  boundary with history intact. Keying on the result — not just the arguments —
  keeps legitimate polling alive: a poll that observes changing state produces a
  distinct signature each step and never trips the guard. Set to `0` to disable.
  See `examples/guardrails`.
- **Structured output (`WithSchema` / `RunOption`).** `Run` and `Stream` now
  accept options; `WithSchema(schema, name, description)` makes the model emit a
  JSON object matching a `fantasy.Schema` instead of free text. `Run` returns the
  raw JSON; `Stream` writes it to the writer.
- **New examples.** `examples/toroid-cli` (minimal CLI) and `examples/toroid-repl`
  (interactive REPL with a dedicated render layer and tests).

### Housekeeping

- Added `.gitignore` rules for compiled example binaries so REPL/CLI build
  artifacts are never committed.

### Notes for upgraders

- `Run` and `Stream` gained a trailing variadic `opts ...RunOption`. Existing
  calls are source-compatible (the variadic is optional).
- `MaxRepeatCalls` defaults to `3`. If your agent legitimately spins the
  synchronous loop on an identical no-progress call (rare), set it to `0` to
  restore the previous unbounded behavior, or have the poll yield/back off
  between checks.
