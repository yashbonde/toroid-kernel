# Changelog

All notable changes to toroid-kernel are documented here. This project follows
[Semantic Versioning](https://semver.org/).

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
