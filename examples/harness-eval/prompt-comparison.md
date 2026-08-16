# Claude Code vs Toroid wire-prompt comparison

> The `~1,910` pi figure above is an **estimate from source**, not a captured
> wire measurement, and should not be compared byte-for-byte with the captured
> Claude/Toroid rows; see the footnote. A full content/strategy comparison
> across all three agents lives in `interop-prompt-analysis.md` in this
> directory.

The exact full captures are published in the private aidocs document
[Claude Code System Prompt](https://aidocs.razorpay.com/app/d/doc_wfmao3n3fn3naruh).

Captured on 2026-07-15 with Claude Code 2.1.210 and the harness's `glm-5p2`
configuration. These are character counts from the JSON request, not token
estimates. Run-specific source, history, and the task prompt are excluded from
the Toroid static-prefix count.

| profile | system | injected messages | tool descriptions | tool schemas | tools | static text total |
|---|---:|---:|---:|---:|---:|---:|
| Claude, configured user environment | 7,572 | 16,365 | 59,093 | 24,049 | 29 | 107,079 |
| Claude `--safe-mode` | 5,175 | 7,937 | 57,302 | 22,812 | 27 | 93,226 |
| Claude `--bare` | 1,593 | 471 | 86 | 2,685 | 3 | 4,835 |
| pi (estimated from source) | ~1,910 | 0 | 0¹ | 0¹ | 4 | ~1,910 |
| Toroid working tree | 5,269 | 0 | 3,178 | 2,483 | 9 | 10,930 |

¹ pi has no separate tool-description/schema payload: tools are a one-line
snippet list embedded inside the system text itself, with no JSON Schema on the
wire. The row is sized from `system-prompt.ts` (standard read/bash/edit/write
toolset + its docs/guidelines block); treat it as an estimate, not a captured
wire measurement.

The configured Claude probe reported 22,979 input tokens on its first real
request. It was not a vanilla Claude Code prompt: global instructions added an
RTK reminder, and the SessionStart hook injected a 13.8 KB Ponytail prompt plus
agent and skill inventories. `--safe-mode` removes those custom instructions,
hooks, and MCP servers, but the built-in tool catalog remains the dominant
payload. `--bare` is much smaller but is a materially different harness with
only Bash, Edit, and Read available.

pi was not captured in the same harness run, so its row is an estimate derived
from `system-prompt.ts` rather than a measured request. pi trims its prompt to
the tools actually wired up, keeps its system text small (~1.9 KB with the four
default tools), and embeds tool one-liners in the prose instead of a separate
schema catalog — the mirror image of Claude's tool-catalog-heavy payload and a
middle path between Claude and Toroid.

## Configured Claude tool catalog

| tool | description chars | schema chars |
|---|---:|---:|
| Agent | 1,446 | 1,290 |
| Bash | 1,282 | 1,393 |
| CronCreate | 2,595 | 878 |
| CronDelete | 100 | 206 |
| CronList | 60 | 119 |
| DesignSync | 3,724 | 5,124 |
| Edit | 360 | 552 |
| EnterWorktree | 3,220 | 687 |
| ExitWorktree | 1,923 | 481 |
| LSP | 1,235 | 1,002 |
| Monitor | 6,359 | 1,189 |
| NotebookEdit | 619 | 940 |
| PushNotification | 1,362 | 330 |
| Read | 790 | 740 |
| ReportFindings | 574 | 1,383 |
| ScheduleWakeup | 2,933 | 1,042 |
| SendMessage | 778 | 426 |
| Skill | 1,713 | 327 |
| TaskCreate | 2,146 | 573 |
| TaskGet | 732 | 214 |
| TaskList | 998 | 119 |
| TaskOutput | 1,049 | 432 |
| TaskStop | 378 | 366 |
| TaskUpdate | 2,243 | 1,095 |
| WaitForMcpServers | 556 | 235 |
| WebFetch | 374 | 315 |
| WebSearch | 307 | 468 |
| Workflow | 18,997 | 1,775 |
| Write | 240 | 348 |

## What Claude's prompt does well

- It gives one crisp action trigger: once enough information exists, act and do
  not re-derive established facts.
- It asks the model to match surrounding naming, comments, and idiom.
- Tool contracts state enforced behavior precisely, especially read-before-edit,
  bounded reads, and no redundant re-read after a successful edit.
- Dynamic system reminders and hooks carry situational policy instead of putting
  every rule in the permanent base prompt.
- The initial git snapshot removes one discovery call, though it becomes stale
  and costs cache reads on every later request.

Toroid already contains the first, third, and fourth ideas in stronger coding-
specific form: bounded inspection, no redundant reads, an eight-turn dynamic
exploration guard, relative-path normalization, and a finish checklist. It also
has a much smaller and more relevant tool catalog.

The working prompt now also explicitly asks code to match surrounding naming,
API/error patterns, and comment density. The Bash tool text was corrected to
match its actual 12 KB truncation boundary, avoiding a stale wire contract.

## Highest-value next experiments

1. Capture both harnesses for the same short task and plot request tokens by
   turn. Toroid's smaller initial prefix but larger aggregate cache reads point
   to accumulated tool results/history, not its static system prompt.
2. A/B test a shorter Toroid prompt rather than adding more rules. Merge the
   overlapping Investigate/Edit/Validate bullets and retain only observable
   decision rules; target roughly 3 KB without dropping the finish contract.
3. Add schema-level `additionalProperties: false` and numeric bounds. Claude's
   schemas constrain malformed tool calls at the wire layer; prose should not do
   work JSON Schema can enforce.
4. Standardize path argument names across read/write/edit/multiedit. Claude uses
   one `file_path` spelling; Toroid currently mixes `filePath` and `path`.
5. Keep control feedback dynamic. Add a one-time reminder only when validation
   is repeated without intervening edits or when the model attempts to finish
   while explicit delivery work remains. Static reminders charge every turn.
6. For reproducible future Claude comparisons, record both stock `--safe-mode`
   and configured-environment results, or choose one and state it in the report.
   The previous numbers include local hooks/plugins and are machine-specific.

Exact bodies are intentionally not checked in: they contain full conversation
history and may contain source. Use `HARNESS_EVAL_CAPTURE=1`; trace files are
written mode `0600` under the ignored results directory.
