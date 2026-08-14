# Conversation hierarchy terminology

Canonical names for conversation structure in toroid (product language).
Use these in docs, Langfuse, cost accounting, and design discussions.

```text
transcript ── a full conversation (one Langfuse session)
 └─ chat ── one human prompt → agent turns → final AI response
    └─ turn ── one tool call + its tool response (a loop step)
       └─ llm-step ── one LLM call (one litellm_request)
```

---

## Definitions

### transcript

A **full conversation** with a user (or host) over time: system setup, many
chats, compaction boundaries, subagent work that belongs to the same
conversation, and session-level totals.

| Property | Meaning |
|----------|---------|
| Starts | First `Run` / `Stream` on a root kernel (or explicit resume of a saved session) |
| Ends | Host closes the kernel / abandons the session; may span many chats |
| Observability | **One Langfuse session** (and, in store terms, typically one root conversation graph) |
| Cost | Cumulative `RunningCostUSD` / session spend for the whole conversation |

A transcript can contain multiple chats (the user keeps talking). Compaction
collapses history *inside* a transcript; it does not start a new transcript
unless the host deliberately opens a new session.

### chat

**One human prompt → zero or more agent turns → final AI response** that is
returned for that prompt (and any mid-chat queue injections that complete
before the chat finishes).

| Property | Meaning |
|----------|---------|
| Starts | Host calls `Run` / `Stream` with a user prompt (or `Wake` processing queued work as a new chat-like unit) |
| Ends | The agentic loop for that prompt stops (no more tool use / max steps / error) and the final assistant text (or structured object) is produced |
| Contains | One or more **turns**; each turn may contain one or more **llm-steps** |
| Cost | Sum of all llm-steps in all turns of this chat |

A chat is the unit “the user asked something and got an answer,” not a single
LLM HTTP call.

### turn

**One tool-loop step**: the model decides to call tool(s), those tools run,
and their results are available for the next step. In the simplest case that
is **one tool call + its tool response**; a single model step may emit several
tool calls in parallel, which still count as **one turn** (one loop iteration).

| Property | Meaning |
|----------|---------|
| Corresponds to | One Fantasy / agent **step** (`OnStepFinish` boundary) |
| Contains | The **llm-step**(s) that produced the assistant content for this step, plus tool execution (local, not an LLM call) |
| Ends | Tools finished and results are appended; ready for the next turn or final reply |
| Cost | Cost of the llm-step(s) in this turn (tool runtime is not model $) |

If the model finishes with **no** tool calls (final answer only), that last
loop step is still a turn: it is a turn with zero tool calls and one (or more)
llm-steps.

### llm-step

**One LLM call** — a single provider/gateway request/response (stream or not).
In a LiteLLM world this is **one `litellm_request`**.

| Property | Meaning |
|----------|---------|
| Examples | One `stream`/`generate` completion; one compact summarize call; one `GenerateObject` structured pass |
| Contains | Token usage (input / output / reasoning / cache) and estimated or gateway cost for **that** request only |
| Does not contain | Tool execution wall time (tools run outside the LLM request) |

A turn usually has **one** llm-step (one model call that may request tools). A
chat can have many turns, hence many llm-steps. Some chats add extra llm-steps
outside the tool loop (e.g. compaction mid-chat, final structured-output call).

**Why not “trace”?** OpenTelemetry and the SQLite store already use *trace* for
conversation/distributed graphs (`TraceID`). Product language uses **llm-step**
only for a single LLM request so those never collide.

---

## Tree (example)

User says “fix the tests,” agent greps, reads a file, runs bash, then answers:

```text
transcript                          # whole REPL/session / Langfuse session
 └─ chat                            # this user message → final answer
     ├─ turn 1                      # model asks for grep
     │    └─ llm-step 1             # LLM call → tool_use grep
     │    (+ tool: grep result)     # not an llm-step
     ├─ turn 2                      # model asks for read
     │    └─ llm-step 2
     │    (+ tool: read result)
     ├─ turn 3                      # model asks for bash
     │    └─ llm-step 3
     │    (+ tool: bash result)
     └─ turn 4                      # model final text, no tools
          └─ llm-step 4
```

If `WithSchema` runs after tools:

```text
 └─ chat
     ├─ turn 1..N                   # agentic llm-steps + tools
     └─ (post-loop) llm-step N+1    # GenerateObject — still part of this chat
```

If auto-compact fires mid-chat:

```text
 └─ chat
     ├─ turn 1..k
     ├─ llm-step (compact)          # summarize history — LLM call, not a user chat
     └─ turn k+1..                  # continues same chat on reduced history
```

---

## Mapping to toroid implementation (approximate)

Product terms do not always match historical code names. Prefer the product
terms in new docs; use this table when reading the codebase.

| Product term | Typical code / infra mapping |
|--------------|------------------------------|
| **transcript** | Root kernel lifetime; SQLite **traces** table / shared `TraceID` for the conversation tree; **one Langfuse session** |
| **chat** | One `Kernel.Run` / `Stream` (or a `Wake` cycle that drives a full answer); gateway **chat** grouping via `WithGatewayTrace` / `x-litellm-session-id` where used |
| **turn** | One agent loop **step** (`OnStepFinish`, `StepUsage` entry); may include multiple parallel tool calls |
| **llm-step** | One LLM HTTP/stream call (`litellm_request`); usage in `EventTurnCost` is usually **per step** and often 1:1 with one LLM call |

Turn hooks bracket that boundary: `TurnStarted` fires before its LLM step;
exactly one of `TurnCompleted` or `TurnFailed` terminates it. Tool execution
inside the turn remains observable through `PreToolUse`, `PostToolUse`, and
`PostToolUseFailure`.

### Request metadata IDs

Every outbound LLM request includes this wire-level hierarchy so gateways can
reconstruct their own conversation model:

```json
{"metadata":{"transcript_id":"…","chat_id":"…","turn_id":"…","trace_id":"…"}}
```

`transcript_id` is stable across the root conversation graph, `chat_id` across
one `Run`/`Stream`/`Wake`, and `turn_id` across one loop iteration. The wire
`trace_id` is fresh for each LLM request. That last field keeps the requested
external schema; internally the same unit is called an **llm-step**, while
kernel/OTEL `TraceID` retains its conversation-graph meaning.

### Reserved “trace” (not product hierarchy)

These keep existing engineering names; they are **not** an llm-step:

| Term | Meaning |
|------|---------|
| OpenTelemetry **trace** | Distributed request graph |
| SQLite / kernel `TraceID` | Conversation-level id for a **transcript** tree (root + subagents) |
| SQLite `traces` table | Metadata for that conversation graph |

---

## Cost and observability rollup

```text
transcript.cost  = sum(chat.cost)
chat.cost        = sum(turn.cost)  [+ compact / schema llm-steps in that chat]
turn.cost        = sum(llm-step.cost)   # usually one LLM call
llm-step.cost    = CalculateCost(usage) or gateway header when available
```

| Layer | Natural metrics |
|-------|-----------------|
| transcript | Session total $, total tokens, Langfuse session |
| chat | Latency user-visible, steps, $ for this prompt |
| turn | Tool name(s), tool latency, step $ |
| llm-step | Model id, usage breakdown, cache hit, `litellm_request` / call id |

See also the README's "Cost accounting" section for how dollars are
estimated per llm-step.

---

## Subagents

A subagent runs its **own chats** (and turns / llm-steps) under the parent
**transcript** (same conversation / Langfuse session tree, distinct span).

```text
transcript
 ├─ chat (parent)
 │    └─ turn (subagent tool)
 │         └─ … parent blocked or async id returned …
 └─ chat(s) (child kernel)     # child Run(task) …
      └─ turn / llm-step …
```

Child spend rolls into transcript totals; product UIs should still attribute
llm-steps to the child chat where possible.

---

## Quick reference

| Term | One line |
|------|----------|
| **transcript** | Full conversation; one Langfuse session |
| **chat** | One human prompt → final AI response |
| **turn** | One tool-loop step (tool call(s) + result(s), or final step with no tools) |
| **llm-step** | One LLM call (`litellm_request`) |
