# Pi / Claude Code / Toroid system-prompt comparison

Companion to `prompt-comparison.md`, which compares *wire sizes* (character /
token counts) captured from live runs on the harness. This document compares
the *content and construction strategy* of the three system prompts:

- **pi** — prompt source read from
  [`earendil-works/pi` `packages/coding-agent/src/core/system-prompt.ts`](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/system-prompt.ts)
- **Claude Code** — stock system prompt as captured on the wire (Anthropic's
  official CLI). The exact capture used for the size table is in the private
  aidocs document linked from `prompt-comparison.md`. General markers below
  match the published prompt.
- **Toroid** — the kernel's own prompt, assembled in
  `prompt_compiler.go` (`buildSystemPrompt` + `toolDescription`).

## Comparison table

| axis | pi | Claude Code | Toroid |
|---|---|---|---|
| Vanilla system text | ~0.6 KB prose, then tool/guideline lists | ~7.6 KB (configured) / 5.2 KB (`--safe-mode`); largely behavioral policy + agent sub-prompt | ~5.3 KB static prefix, compiled once from discovered capabilities |
| Tool catalog | prose one-liners; no JSON Schema on the wire | 29 tools, each with long prose description **and** JSON Schema | 9 tools, short prose contracts; JSON Schema defined in registry |
| Construction | function of `selectedTools`/`toolSnippets`; only listed tools get a one-line snippet | giant static catalog; dynamic blocks (session hooks, skills) injected per request | capability list (skills/MCP/subagents) compiled once at startup; stable, cache-friendly |
| Guidelines model | additive bullets derived from which tools are present | large "must/must not" behavioral policy block | compact observable decision rules, coding-specific |
| Caching strategy | not surfaced here | Anthropic `cache_control` breakpoints on system + last message | native `cache_control` breakpoints (Claude route) + deterministic stable prefix |
| Dynamic policy | via prompt-templates/extensions on request | via SessionStart hooks and injected reminders | via persistent system prompt + step-loop nudges (eight-turn exploration guard, finish checklist) |

## How pi builds its prompt

pi's `buildSystemPrompt` is modular and *minimal by construction*:

- The base prose is small: a one-line identity ("You are an expert coding
  assistant operating inside pi") and a tiny "Available tools:" list.
- Tools appear **only if the caller passes a one-line snippet**; otherwise they
  are omitted entirely, so the prompt cannot drift from what is wired up.
- Guidelines are *derived from the tool set*, e.g. when only `bash` is present
  it adds "Use bash for file operations like ls, rg, find" — it does not pair a
  separate `grep`/`find`/`ls` tool list with a redundant instruction.
- Always-on behavior is two bullets: "Be concise in your responses" and "Show
  file paths clearly when working with files".
- Project context (`<project_instructions>`), skills, and append text are
  appended as distinct sections, keeping the static core small.

Net effect: pi optimizes for *a tiny, tool-driven prompt* — most context is
deferred or derived rather than baked in.

## How Claude Code builds its prompt (as captured)

Claude Code's installed prompt is dominated by a large static behavioral
catalog plus a broad tool set:

- A long tone/style block: "be concise", "answer in fewer than 4 lines",
  "minimize output tokens", "no preamble/postamble", "avoid comments unless
  asked", "never commit unless asked", "run lint/typecheck when done".
- A tool-catalog-heavy payload: in the configured harness capture the tool
  descriptions alone were ~59 KB with ~24 KB of JSON Schema across 29 tools.
- It does include good coding discipline: one crisp "act once you have enough
  information; do not re-derive established facts", match surrounding naming/
  idiom, and read-before-edit contracts.
- Much situational policy arrives *dynamically* — via SessionStart hooks,
  injected reminders, and tool-contract prose — rather than in the base prompt.

The earlier audit (`prompt-comparison.md`) concluded toroid already carries
Claude Code's best ideas (stable prefix, bounded reads, no redundant re-read,
finish checklist) in a much smaller, more relevant catalog.

## How Toroid builds its prompt (`prompt_compiler.go`)

- Static prefix is a small, stable block: identity, working directory + date,
  and ~10 coding decision rules (read-before-edit, search-once, bounded
  inspection, no redundant validation, finish checklist).
- Capabilities (skills, MCP, optional subagents) are **compiled once** at
  startup into the prefix, precisely so the prompt is cache-stable — skills
  list only frontmatter; full bodies load on demand via the `skill` tool.
- Tool contracts are per-tool prose in `toolDescription`, e.g. bash = non-
  interactive, 120s timeout, 12 KB truncation; read = <=2000 lines, 5 MiB.
- Delegation section only appears when subagents are enabled; model names are
  printed for primary/smaller model when a compact model is set.
- Runtime nudges (exploration guard, repeated-validation reminder) live in the
  step loop, not the permanent prompt.

## Where toroid sits

| property | pi | Claude Code | toroid |
|---|---|---|---|
| Prompts trimmed to enabled tools | **yes** (only snippet-provided tools) | no (full catalog) | **yes** (skills/MCP/subagents opt-in) |
| Static prompt size | smallest | largest | middle, but much smaller than configured Claude |
| Coding-specific discipline encoded | thin | broad imperative block | strong, focused decision rules |
| Schema-level enforcement | none on the wire | **yes**, `additionalProperties:false` etc. | prose contracts + registry schema (schema enforcement a listed next step) |
| Cache-stable prefix | modular | has caching but large catalog | intentional, deterministic ordering |

Current gaps / next experiments already tracked in `prompt-comparison.md`:
standardize path argument naming (`filePath` vs `path`), consider
`additionalProperties: false` / numeric bounds in schemas, and keep control
feedback dynamic rather than charging every turn.

*Sources: pi `system-prompt.ts` (fetched from upstream `earendil-works/pi`);
Claude Code system prompt as captured on the wire (aidocs capture referenced in
`prompt-comparison.md`); toroid `prompt_compiler.go` in this repo.*