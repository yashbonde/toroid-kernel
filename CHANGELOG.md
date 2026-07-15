# Changelog

All notable changes to toroid-kernel are documented here. This project follows
[Semantic Versioning](https://semver.org/).

## Unreleased

## v0.5.0

### Added

- **Capability-loading end-to-end fixture.** `examples/e2e-test` runs offline
  with a committed mock skill and an in-process streamable-HTTP MCP server. It
  invokes skill, MCP, and host tools, covers structured output and event
  round-tripping, and asserts that the compiled system/tool cache prefix stays
  byte-stable across turns and later runs.
- **Always-on OTEL transcript.** Every session now appends its observable events
  (tool calls included) to `~/.toroid/sessions/<session-id>/transcript.jsonl` as
  newline-delimited, OTEL-shaped records (spec-valid trace/span IDs + the
  canonical `Event.OTEL()` projection). It is independent of `Save`/SQLite, so a
  durable, greppable trace of every run exists by default. Display-only events
  (reasoning deltas, idle/queue bookkeeping) are excluded, matching the OTEL
  span-event filter. New helper `SessionDir(sessionID)`.

### Changed

- **Hardened the bash tool against hangs.** Commands now run non-interactively
  (`EDITOR`/`GIT_EDITOR`/`PAGER` neutralized, `GIT_TERMINAL_PROMPT=0`, empty
  stdin) so `git commit` (no `-m`), `git rebase -i`, `crontab -e`, etc. can no
  longer open an editor and block the kernel. Every command also gets a timeout
  (default 120s, overridable via the new `timeout` arg on the bash tool) and is
  launched in its own process group, killed as a group on timeout so orphaned
  grandchildren (e.g. an editor holding `/dev/tty`) die too.

- **`examples/toroid-cli` merged into `examples/cli` (was `examples/repl`).**
  The one-shot NDJSON runner is now the `--run` flag of the interactive
  runner: `go run ./examples/cli --run '<prompt>' [--plain]` replaces
  `go run ./examples/toroid-cli`. Because `--run` shares the REPL's flag set,
  `--model`, `--thinking`, and `--save` all apply. One binary now does both
  jobs — interactive chat and the machine-facing event stream for non-Go hosts.
- **grep, glob, and ls tools removed.** bash+rtk covers all three (`rtk grep`,
  `rtk find`, `rtk ls` — compressed output), and `read` already lists
    directories. The default toolset is now read/write/edit/multiedit/bash;
    skills are discovered on demand and subagent tools are explicit opt-in.
- **`MaxIter` default 25 → 100.** With prompt-cache breakpoints on every loop
  step, additional turns re-read the prefix at cache price, so a deep tool loop
  is affordable; the repeat-call guard still stops genuine spins early.

- **Tool layer revamp** (informed by a study of crush, opencode, and pi):
  - Startup capabilities are compiled once after skill and MCP discovery. The
    capability reminder lives once in the stable system prefix, and deterministic
    tool ordering keeps the reusable prompt-cache prefix unchanged across turns.
  - Tool docs are compiled alongside the system prompt in `prompt_compiler.go`:
    short contracts, no frontmatter parser, and no override files to drift from
    actual tool behavior.
  - **todo tools removed** (`todo_write`/`todo_read` + SQLite table): the
    system prompt now tells the model to keep plans in a markdown file.
  - **Truncated tool output spills to `~/.toroid/sessions/<session>/tool-output/<ts>.txt`** and
    the truncation note names the file, so the model (or a subagent) can read
    or grep the full result instead of losing it.
  - **rtk integration**: when the `rtk` CLI is on PATH, simple read-only bash
    commands (git status/diff/log, ls, cat, grep, find, …) are transparently
    rewritten to `rtk <cmd>` for token-compressed output.
  - **Deterministic tool ordering**: the wire tool list is sorted by name —
    the registry map previously shuffled it every Run, changing the request
    prefix and busting the prompt cache across Runs.
  - **glob fixed**: `find -name` matches basenames only, so the documented
    `**/*.go` form silently matched nothing; the `**/` prefix is now handled.
  - `EventPermissionRequest` and `PermissionPayload` deleted — the event was
    never fired and implied a permission layer that does not exist.
  - Validation errors name the tool and instruct a schema-conformant re-call
    in one line.
- **`assets/pre-ship.md`**: checklist AI agents must follow before pushing
  (verify live, refresh README numbers, changelog, scrub secrets/scratch).

### Added

- **Hard USD spend limits.** Hosts can cap a single `Run`/`Stream` call with
  `WithMaxTurnSpendUSD` and cumulative transcript spend with
  `Config.MaxTranscriptSpendUSD`. Once accounting reaches either limit, the
  kernel prevents further LLM steps, including structured-output and wake
  re-entry; subagents inherit only the parent's remaining transcript budget.
- **`anthropic/` provider (native messages API).** An `anthropic/<model>` id
  talks to `api.anthropic.com/v1/messages` (`ANTHROPIC_API_KEY`) via a native
  wire in the llm package (`AnthropicClient`, sharing the `Chat` interface and
  retry logic with the OpenAI-compatible client): system as a top-level block,
  tool results inside user messages, tool_use blocks, consecutive-role
  merging, image/document blocks, thinking via `budget_tokens`, and SSE
  streaming. Explicit `cache_control` prompt caching is wired and brutally
  live-verified (cache write turn 2 → read turn 3 → cross-Run read of 11.8k
  tokens at 3 fresh input tokens). Anthropic's OpenAI-compat endpoint was
  rejected because it hides cache accounting entirely.
- **In-code price table.** Per-token rates for the Claude and GPT-5 families
  are cached in `model.go` (`Model.Price`) and price any llm-step the gateway
  did not (direct provider routes, streaming). Providers expose no pricing
  API, so the table is code — unknown families stay honestly unpriced.
- **`openai/` provider.** An `openai/<model>` id talks straight to the OpenAI
  API (`OPENAI_API_KEY`, base `https://api.openai.com/v1`) — same
  OpenAI-compatible wire, no gateway required. Live-verified with
  `openai/gpt-5.4-mini`: tool loop, structured output, SSE streaming, and
  image input. Cost is unknown on this route (only LiteLLM reports the
  per-call cost header); token usage stays exact and unbilled steps are logged.

### Changed

- **Adversarial cost audit + fixes** (report:
  `assets/adversarial-cost-review.md`, all 15 findings resolved). Highlights:
  prompt-cache breakpoints (`cache_control`) on Claude-family loop steps
  (live-verified: later turns read the whole prefix at cache price); per-turn
  history pruning removed (it rewrote already-sent messages and busted the
  cache prefix every turn — trimming now happens only at compaction, which also
  strips media and reasoning); per-session usage is summed, fixing subagent
  cost rollup that previously counted only the child's last llm-step; read-tool
  media capped at 5 MiB and text output truncated like every other tool;
  subagent output capped; tool-result media never sent to text-only models;
  media bytes no longer persisted into SQLite event payloads; loop-guard
  signatures hashed; unbilled llm-steps logged; system-prompt date is
  day-granular.

- **Gateway-truth cost only.** The local pricing model (`pricing.go`,
  `assets/pricing.json`, `assets/usd_x.json`, `GetModelPricing`,
  `CalculateCost`, currency conversion) is deleted. Every non-streaming
  llm-step bills the gateway's `x-litellm-response-cost`; `Usage.PricingOK`
  now means "the gateway reported this cost". `Model` no longer carries rates.
- **OnPayload removed.** `Config.OnPayload` / `StepOptions.OnPayload` /
  `PayloadDebug` are gone; the kernel fires an `EventLLMStep` event
  (`LLMStepPayload`: model, message count, tool names, schema) before each
  outbound llm-step instead.
- **Client retries.** `llm.Client` retries transient failures (network errors,
  429, 5xx) up to three times with exponential backoff — previously a mid-chat
  gateway 502 killed the whole run.
- **Shallow-function purge.** Inlined or deleted single-use micro-helpers
  across the kernel, client, and examples (`cheapStep`, `cheapModelID`,
  `stepOptions`, `windowTokens`, `setWindowTokens`, `trimAllToolResults`,
  `FireTraceLog`, `pathToTilde`, `parseCostHeader`, `hasFiles`, `scanSSE`,
  `apiKeyEnvFor`, custom `trimSpace`, …); fixed a double window-gauge update
  per turn in the step loop.
- Runners (`toroid-cli`, `repl`) default to
  `llmgateway/claude-haiku-4-5` and authenticate with `LLM_GATEWAY_KEY` only.

- **Fantasy removed; in-repo `llm` package is the only wire.** The
  `charm.land/fantasy` dependency (and its provider zoo: Anthropic, Google,
  OpenAI SDKs, AWS/Bedrock, genai, …) is gone. The new `llm/` package owns the
  data model (`Message`/`Part`), the OpenAI-compatible chat-completions client
  (LiteLLM gateway only: SSE streaming with usage-in-stream, tool calls,
  multimodal content blocks, `x-litellm-response-cost` capture, traceparent /
  session headers), tool abstraction (`ToolHandler`, reflection JSON-schema
  generation), and JSON round-tripping for history persistence.
  - The kernel-owned step loop (`steploop.go`) is now the only chat loop —
    `Config.UseStepLoop` and `Config.Provider`/`PromptCache` are gone; caching
    is gateway/upstream-managed.
  - `FantasyStep` is replaced by `GatewayStep` over `llm.Client`; `FauxStep`
    stays as the scripted test backend. Structured output (`WithSchema`, now
    `toroid.Schema` = JSON-schema map + `toroid.GenerateSchema`) runs as a
    forced tool call, which works across LiteLLM upstreams including
    Bedrock-backed Anthropic.
  - `Config.Thinking` maps to gateway `reasoning_effort` (low/high); reasoning
    tokens are billed and split out in `Usage`.
  - Tool results can carry media (`llm.ToolResult.Files`): the read tool
    returns images/PDFs as content blocks a vision model can see.
  - Defaults are gateway-first: `Model` defaults to
    `llmgateway/claude-haiku-4-5`, `APIKey` falls back to `$LLM_GATEWAY_KEY`.

### Added

- **`Config.SmallerModel`.** Optional cheaper `provider/model` used for
  conversation compaction and subagents (sync + async). When set, the system
  prompt documents primary vs secondary model routing so the agent prefers
  `subagent` for exploratory work. Nested subagents stay on the secondary tier.
- **`Config.PromptCache` (default true).** Requests Anthropic-style ephemeral
  `cache_control` breakpoints via Fantasy `PrepareStep` (system + last two
  messages) and on the last registered tool definition. No-op for providers
  that ignore the option.
- **Shared tool output cap (`tools.MaxToolOutputChars` = 20k).** `grep`,
  `glob`, `ls`, `skill`, MCP results, and `bash` all pass through
  `TruncateToolOutput` so one fat result cannot dominate the next prompt.

### Changed

- **Tighter cost defaults.** `MaxIter` 50→25, `ToolCallPrunedSize` 40k→20k,
  `TotalContextSize` 300k→200k, `CompactionBufferSize` 30k→50k (earlier
  auto-compact). Zero values in `NewKernel` now apply these same floors.

### Fixed

- **Double system prompt.** System text is owned only by Fantasy
  `WithSystemPrompt`; it is no longer also appended into `History` (which was
  double-billing every step and busting a stable cache prefix).
- **Compaction cost accounting.** Compact LLM calls now run through
  `recordUsage` (and prefer `SmallerModel` when set). Fat tool results are
  trimmed before the summarize call.
- **Subagent cost rollup.** Child session costs are added into the parent
  `runningCostUSD` after `RunSubagent` returns.
- **Unbounded tool results.** Broad `grep`/`glob`/`ls`/MCP/`skill` outputs
  were previously unlimited and re-entered context at full size.

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
  See `examples/running --guardrails`.
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
