# Benchmarking toroid-kernel

This document tracks (1) which benchmarks we'll use to evaluate toroid as an
agent harness, and (2) a feature-by-feature comparison against the other
harnesses in the space, so gaps are visible and prioritizable.

## 1. Benchmarks we're adopting

toroid is a Go library that drives tool-using LLM agents from a terminal /
programmatic context — no browser or GUI tool surface. That rules out
browser/OS-control benchmarks and narrows the useful set to terminal- and
tool-calling-shaped evals.

| Benchmark | What it measures | Status | Why |
|---|---|---|---|
| **Terminal-Bench** | Multi-step CLI/terminal task completion | **Adopt (primary)** | Directly matches toroid's shape: a kernel driving Bash/Read/Edit/Grep-style tools in a terminal. |
| **Tau-bench** | Multi-turn tool-calling correctness in simulated domains | **Adopt (primary)** | Tests exactly what `tools/registry.go` exists for — reliable multi-turn tool dispatch and state tracking. |
| **Harness-Bench** methodology | Isolates harness effect (same model, different scaffolds) | **Adopt (methodology)** | Not a task set to run standalone — it's the right *comparison method*: pin one model, run it through toroid vs. Claude Code / Codex CLI on the same Terminal-Bench/Tau-bench tasks, and diff the scores. This isolates what toroid's design contributes, independent of model quality. |
| **SWE-bench Verified** | Real GitHub issue resolution (patch + test pass) | **Secondary** | Only relevant if/when toroid targets coding-agent workloads specifically; requires wrapping toroid with a repo + test-runner harness. |
| **Aider Polyglot** | Multi-language code-editing accuracy | **Secondary** | Cheaper than SWE-bench to wire up; good regression check for `tools/edit.go` / `tools/multiedit.go`. |
| **AgentBench** | Multi-environment (OS, DB, web shopping, etc.) | **Not adopted (partial fit)** | Only terminal/OS/DB sub-tasks would apply; would require cherry-picking tasks rather than running the suite. |
| **GAIA** | Multi-step reasoning + tool-use Q&A | **Not adopted (partial fit)** | Many tasks assume web browsing/file retrieval outside toroid's current tool surface. |
| **WebArena** | Browser-based web tasks | **Not adopted** | Requires a browser-control tool toroid doesn't have. |
| **OSWorld** | Full-OS GUI control | **Not adopted** | Requires mouse/keyboard/screen tools — out of scope for a terminal kernel. |

**Plan:** run Terminal-Bench and Tau-bench task suites through toroid with a
pinned model, then run the same tasks through Claude Code and Codex CLI with
the same model, and diff scores (Harness-Bench methodology). That tells us
whether toroid's tool design/context management helps or hurts, independent
of the underlying model.

## 2. Feature comparison

Reference points as of mid-2026. "toroid" reflects the current state of this
repo (`kernel.go`, `tools/`, `provider.go`, `events.go`, `skills.go`,
`tools/mcp.go`, `tools/skill.go`, `examples/toroid-cli/`).

| Feature | toroid | Claude Code | Claude Agent SDK | OpenAI Codex CLI | pi.dev |
|---|---|---|---|---|---|
| **Distribution** | Go library, embed in your own program; plus `toroid-cli` binary (`examples/toroid-cli/`) that emits NDJSON events on stdout — usable as a subprocess from any language | Standalone CLI / IDE ext / desktop app | Go/Python/TS SDK, embed in your own program | Standalone CLI (Rust) / IDE ext / desktop app / cloud | Standalone CLI + TUI, npm/git packages |
| **Core loop** | Kernel `Run`/`Stream`, tool-call loop via `charm.land/fantasy` | Built-in agentic loop | Exposes the same loop Claude Code runs on, programmatically | Built-in agentic loop | Minimal agent loop, four core tools (read/write/edit/bash) |
| **Model providers** | Anthropic, Google, OpenAI, OpenAI-compatible gateway (`provider.go`) | Anthropic only (native); some third-party model routing | Anthropic only | OpenAI only (native) | Unified LLM API — many providers |
| **Built-in tools** | Bash, Read, Write, Edit, MultiEdit, Glob, Grep, LS, Todo, Notify, Subagent, Skill (`tools/`) | Bash, Read, Write, Edit, Glob, Grep, WebFetch, WebSearch, Task/Agent, TodoWrite, NotebookEdit, etc. | Same tool surface as Claude Code, programmatically composable | Bash/exec, file read/write/patch, web search (cloud mode) | Deliberately just 4: read, write, edit, bash — everything else is opt-in |
| **MCP client support** | Yes — `tools/mcp.go` connects via streamable HTTP (SSE fallback), discovers tools via `tools/list`, registers them as namespaced `ToolDef`s in `tools.Registry`. Wired in `kernel.go` via `Config.MCPServers`. No stdio transport; non-text content (images, embedded resources) dropped | Yes — full MCP client (stdio/SSE), first-class | Yes — inherits Claude Code's MCP client | Yes — MCP client support | **None by design** — "MCP is overkill"; favors CLI tools + README-based progressive disclosure over MCP |
| **Skills / progressive-disclosure capability packages** | Partial — `skills.go` discovers `~/.toroid/skills/*.md` by frontmatter (name + description only in system prompt); `tools/skill.go` loads full body on demand. On by default (`Config.LoadSkills`). No marketplace, no `/skill:name` invocation syntax, no hook composition | Yes — Skills marketplace, `/skill:name`, loaded on demand, composes with hooks | Yes (same primitive, programmatic) | Partial — custom instructions/prompts, no first-class skills marketplace | Yes — first-class Skills concept, invoked via `/skill:name`, explicitly built for progressive disclosure without busting prompt cache |
| **Subagents** | Yes — `RunSubagent`, sync and async (`subagent_async`) background agents that wake an idle kernel on completion | Yes — Task tool / Agent Teams | Yes — same primitive, programmatic | Yes — multi-agent v2 support | Yes — sub-agents supported, not built into core |
| **Background / async agents** | Yes — native concept (`MasterIdle`, `TaskCompleted` events) | Limited — mostly foreground; cloud tasks for async | Programmatic — caller controls concurrency | Yes — cloud tasks run in OpenAI-managed containers | No first-class async agent primitive |
| **Persistence** | Single-file SQLite (traces, costs, events, todos), OTEL-compatible span IDs (`store.go`) | Session transcripts, local project state | Caller-managed (SDK doesn't prescribe storage) | Session transcripts; cloud tasks persisted server-side | Minimal / caller-managed |
| **Telemetry / tracing** | Built-in OTEL export (`otlp.go`), W3C traceparent propagation, Langfuse-compatible | Built-in usage/cost tracking; limited OTEL | Caller wires their own | Some usage tracking; less OTEL-native | Minimal |
| **Cost accounting** | Built-in, pricing table in `assets/pricing.json` + `pricing.go` | Built-in cost/usage display | Billed via separate Agent SDK credit pool at API rates | Built-in usage tracking | Caller-managed |
| **Hooks / lifecycle events** | Event bus + `HookRegistry` (`events.go`, `kernel.go`) — kernel-level events (PermissionRequest, MasterIdle, etc.) | Rich hooks system (pre/post tool-use, shell hooks, settings-driven) | Same hooks primitive as Claude Code | Limited hooks | Extension/plugin points, less formalized than Claude Code |
| **Permission / approval gating** | `EventPermissionRequest` exists as an event; no built-in policy engine | Full permission modes, allow/deny lists, settings-driven | Caller implements policy on top of SDK primitives | Sandboxing + approval modes | Permission gates available as opt-in, not built-in |
| **Sandboxing** | None built-in | OS-level sandboxing options on some platforms | Caller-managed | Sandboxed execution (containers) built-in for cloud tasks | Available as opt-in (not core) |
| **Multimodal input** | Yes (`multimodal.go`) | Yes (images, screenshots) | Yes | Yes (image inputs added 2026) | Depends on underlying model/provider |
| **Notifications** | Built-in (`notify` tool + desktop notification sinks, pluggable) | Limited | Caller-managed | Limited | Caller-managed |
| **Extensibility model** | Go package — extend by writing Go code against `tools.Registry`, or plug in external MCP servers via `Config.MCPServers` | Skills, hooks, MCP, plugins, marketplace | Full programmatic control — build any of the above yourself | Extensions via SDK, GitHub/Slack integrations | Packages (extensions, skills, prompts, themes) via npm/git |

### Radar: 7-axis capability comparison

Scored 0–5 per axis from the table above (0 = absent, 5 = first-class/mature).
Axes: Tool Breadth, MCP Support, Skills, Subagents/Async, Persistence &
Telemetry, Permission/Sandbox, Extensibility.

<p align="center">
<svg viewBox="0 0 620 440" xmlns="http://www.w3.org/2000/svg" font-family="ui-sans-serif, system-ui, sans-serif">
  <style>
    svg { color: #333; }
    @media (prefers-color-scheme: dark) {
      svg { color: #ddd; }
    }
  </style>
  <polygon points="220.0,188.0 245.0,200.0 251.2,227.1 233.9,248.8 206.1,248.8 188.8,227.1 195.0,200.0" fill="none" stroke="currentColor" stroke-opacity="0.15" stroke-width="1"/>
  <polygon points="220.0,156.0 270.0,180.1 282.4,234.2 247.8,277.7 192.2,277.7 157.6,234.2 170.0,180.1" fill="none" stroke="currentColor" stroke-opacity="0.15" stroke-width="1"/>
  <polygon points="220.0,124.0 295.1,160.1 313.6,241.4 261.7,306.5 178.3,306.5 126.4,241.4 144.9,160.1" fill="none" stroke="currentColor" stroke-opacity="0.15" stroke-width="1"/>
  <polygon points="220.0,92.0 320.1,140.2 344.8,248.5 275.5,335.3 164.5,335.3 95.2,248.5 119.9,140.2" fill="none" stroke="currentColor" stroke-opacity="0.15" stroke-width="1"/>
  <polygon points="220.0,60.0 345.1,120.2 376.0,255.6 289.4,364.2 150.6,364.2 64.0,255.6 94.9,120.2" fill="none" stroke="currentColor" stroke-opacity="0.15" stroke-width="1"/>
  <line x1="220" y1="220" x2="220.0" y2="60.0" stroke="currentColor" stroke-opacity="0.25" stroke-width="1"/><line x1="220" y1="220" x2="345.1" y2="120.2" stroke="currentColor" stroke-opacity="0.25" stroke-width="1"/><line x1="220" y1="220" x2="376.0" y2="255.6" stroke="currentColor" stroke-opacity="0.25" stroke-width="1"/><line x1="220" y1="220" x2="289.4" y2="364.2" stroke="currentColor" stroke-opacity="0.25" stroke-width="1"/><line x1="220" y1="220" x2="150.6" y2="364.2" stroke="currentColor" stroke-opacity="0.25" stroke-width="1"/><line x1="220" y1="220" x2="64.0" y2="255.6" stroke="currentColor" stroke-opacity="0.25" stroke-width="1"/><line x1="220" y1="220" x2="94.9" y2="120.2" stroke="currentColor" stroke-opacity="0.25" stroke-width="1"/>
  <text x="220.0" y="32.0" text-anchor="middle" font-size="12" fill="currentColor" dominant-baseline="middle">Tool Breadth</text><text x="367.0" y="102.8" text-anchor="start" font-size="12" fill="currentColor" dominant-baseline="middle">MCP Support</text><text x="403.3" y="261.8" text-anchor="start" font-size="12" fill="currentColor" dominant-baseline="middle">Skills</text><text x="301.6" y="389.4" text-anchor="start" font-size="12" fill="currentColor" dominant-baseline="middle">Subagents/Async</text><text x="138.4" y="389.4" text-anchor="end" font-size="12" fill="currentColor" dominant-baseline="middle">Persistence &amp; Telemetry</text><text x="36.7" y="261.8" text-anchor="end" font-size="12" fill="currentColor" dominant-baseline="middle">Permission/Sandbox</text><text x="73.0" y="102.8" text-anchor="end" font-size="12" fill="currentColor" dominant-baseline="middle">Extensibility</text>
  <polygon points="220.0,124.0 295.1,160.1 282.4,234.2 289.4,364.2 150.6,364.2 188.8,227.1 144.9,160.1" fill="#e07a3e" fill-opacity="0.12" stroke="#e07a3e" stroke-width="2"/><circle cx="220.0" cy="124.0" r="2.5" fill="#e07a3e"/><circle cx="295.1" cy="160.1" r="2.5" fill="#e07a3e"/><circle cx="282.4" cy="234.2" r="2.5" fill="#e07a3e"/><circle cx="289.4" cy="364.2" r="2.5" fill="#e07a3e"/><circle cx="150.6" cy="364.2" r="2.5" fill="#e07a3e"/><circle cx="188.8" cy="227.1" r="2.5" fill="#e07a3e"/><circle cx="144.9" cy="160.1" r="2.5" fill="#e07a3e"/><polygon points="220.0,60.0 345.1,120.2 376.0,255.6 275.5,335.3 178.3,306.5 95.2,248.5 119.9,140.2" fill="#5b8def" fill-opacity="0.12" stroke="#5b8def" stroke-width="2"/><circle cx="220.0" cy="60.0" r="2.5" fill="#5b8def"/><circle cx="345.1" cy="120.2" r="2.5" fill="#5b8def"/><circle cx="376.0" cy="255.6" r="2.5" fill="#5b8def"/><circle cx="275.5" cy="335.3" r="2.5" fill="#5b8def"/><circle cx="178.3" cy="306.5" r="2.5" fill="#5b8def"/><circle cx="95.2" cy="248.5" r="2.5" fill="#5b8def"/><circle cx="119.9" cy="140.2" r="2.5" fill="#5b8def"/><polygon points="220.0,60.0 345.1,120.2 376.0,255.6 275.5,335.3 192.2,277.7 126.4,241.4 94.9,120.2" fill="#7fbf7f" fill-opacity="0.12" stroke="#7fbf7f" stroke-width="2"/><circle cx="220.0" cy="60.0" r="2.5" fill="#7fbf7f"/><circle cx="345.1" cy="120.2" r="2.5" fill="#7fbf7f"/><circle cx="376.0" cy="255.6" r="2.5" fill="#7fbf7f"/><circle cx="275.5" cy="335.3" r="2.5" fill="#7fbf7f"/><circle cx="192.2" cy="277.7" r="2.5" fill="#7fbf7f"/><circle cx="126.4" cy="241.4" r="2.5" fill="#7fbf7f"/><circle cx="94.9" cy="120.2" r="2.5" fill="#7fbf7f"/><polygon points="220.0,92.0 320.1,140.2 282.4,234.2 275.5,335.3 178.3,306.5 95.2,248.5 144.9,160.1" fill="#c77dd2" fill-opacity="0.12" stroke="#c77dd2" stroke-width="2"/><circle cx="220.0" cy="92.0" r="2.5" fill="#c77dd2"/><circle cx="320.1" cy="140.2" r="2.5" fill="#c77dd2"/><circle cx="282.4" cy="234.2" r="2.5" fill="#c77dd2"/><circle cx="275.5" cy="335.3" r="2.5" fill="#c77dd2"/><circle cx="178.3" cy="306.5" r="2.5" fill="#c77dd2"/><circle cx="95.2" cy="248.5" r="2.5" fill="#c77dd2"/><circle cx="144.9" cy="160.1" r="2.5" fill="#c77dd2"/><polygon points="220.0,156.0 220.0,220.0 376.0,255.6 261.7,306.5 206.1,248.8 157.6,234.2 94.9,120.2" fill="#e0c93e" fill-opacity="0.12" stroke="#e0c93e" stroke-width="2"/><circle cx="220.0" cy="156.0" r="2.5" fill="#e0c93e"/><circle cx="220.0" cy="220.0" r="2.5" fill="#e0c93e"/><circle cx="376.0" cy="255.6" r="2.5" fill="#e0c93e"/><circle cx="261.7" cy="306.5" r="2.5" fill="#e0c93e"/><circle cx="206.1" cy="248.8" r="2.5" fill="#e0c93e"/><circle cx="157.6" cy="234.2" r="2.5" fill="#e0c93e"/><circle cx="94.9" cy="120.2" r="2.5" fill="#e0c93e"/>
  <circle cx="470" cy="40" r="5" fill="#e07a3e"/><text x="482" y="44" font-size="12" fill="currentColor">toroid</text><circle cx="470" cy="58" r="5" fill="#5b8def"/><text x="482" y="62" font-size="12" fill="currentColor">Claude Code</text><circle cx="470" cy="76" r="5" fill="#7fbf7f"/><text x="482" y="80" font-size="12" fill="currentColor">Claude Agent SDK</text><circle cx="470" cy="94" r="5" fill="#c77dd2"/><text x="482" y="98" font-size="12" fill="currentColor">Codex CLI</text><circle cx="470" cy="112" r="5" fill="#e0c93e"/><text x="482" y="116" font-size="12" fill="currentColor">pi.dev</text>
</svg>
</p>

toroid's polygon makes the shape obvious: strong on Subagents/Async and
Persistence & Telemetry (arguably ahead of everyone else there), now mid-pack
on MCP (HTTP/SSE only, no stdio) and Skills (primitive exists, no marketplace),
and still weak on Permission/Sandbox — the remaining gaps called out below.

### Biggest gaps for toroid, in priority order

1. **Skills / progressive disclosure** — the primitive exists (`skills.go`
   discovers `~/.toroid/skills/*.md`, `tools/skill.go` loads full bodies on
   demand), but there's no marketplace, no `/skill:name` invocation syntax,
   and no hook composition. Both Claude Code and pi.dev treat this as a
   first-class primitive for keeping prompts cache-friendly as capability
   surface grows.
2. **MCP client gaps** — `tools/mcp.go` covers HTTP/SSE transport, tool
   discovery, and namespaced registration, but lacks **stdio transport** and
   drops **non-text content** (images, embedded resources). Adding stdio
   would unlock the large ecosystem of local MCP servers that communicate
   over stdin/stdout.
3. **Permission policy engine** — the `PermissionRequest` event exists but
   there's no built-in allow/deny policy layer on top of it (Claude Code and
   Codex both ship one).
4. **Sandboxed execution** — no isolation for Bash/tool execution; relevant
   if toroid is used for less-trusted/autonomous workloads.

MCP and Skills are not mutually exclusive with each other or with A2A-style
agent-to-agent protocols — they operate at different layers (MCP: agent→tool,
Skills: on-demand capability loading, A2A: agent→agent) and can be layered on
top of the same `tools.Registry` abstraction toroid already has.
