# harness-eval — grounded GLM-5p2 coding suite

This suite runs the same repository coding task through toroid, Claude Code,
and pi, with every harness pinned to the same `glm-5p2` model through the
Razorpay LLM gateway. It compares the harnesses—not different models.

Every run starts from an isolated clone of `master`, measures the full process
tree, and verifies the pushed result from a fresh clone. Task-specific tests are
kept outside the agent checkout and overlaid only by the verifier, so an agent
cannot see or tailor its implementation to them.

## Tasks

```bash
go run ./examples/harness-eval list
```

The initial suite deliberately spans different coding shapes:

| task | primary ability |
|---|---|
| `spend-limit` | cross-cutting kernel feature and deterministic fake-LLM tests |
| `unicode-truncation` | localized Unicode boundary bug fix |
| `registry-concurrency` | synchronization, snapshot semantics, race detector |
| `edit-file-mode` | filesystem correctness and atomic replacement |
| `markdown-media-paths` | parser edge cases and multimodal integration |
| `config-validation` | API validation, defaults, and error design |

Each task lives in `tasks/<id>/`:

- `task.json` declares its branch slug, required changed paths, required files,
  timeout, and verifier command.
- `prompt.txt` states coding outcomes only. The runner adds the common branch,
  build, test, commit, push, log, and return-to-master delivery contract.
- `_verify-overlay/` contains hidden tests copied into the verification clone
  after the agent exits.

## Run

The gateway variables are required:

```bash
export LLM_GATEWAY_BASE_URL=...
export LLM_GATEWAY_KEY=...

# one task through all harnesses
go run ./examples/harness-eval unicode-truncation

# one task through selected harnesses
go run ./examples/harness-eval registry-concurrency toroid claude

# backward-compatible shorthand: original spend-limit task
go run ./examples/harness-eval toroid pi

# re-run verification without another model call
go run ./examples/harness-eval recheck registry-concurrency
```

Run tasks separately rather than launching the whole matrix accidentally: a
full six-task, three-harness suite makes 18 paid agent runs.

The runner also keeps an append-only total-cost ledger across tasks and
defaults to a `$10` suite ceiling. Before each harness it checks the remaining
budget; while a process runs it watches emitted usage and kills the process
group when total cost reaches the remainder. Claude additionally receives
its own `--max-budget-usd` ceiling. Set a lower ceiling with
`HARNESS_EVAL_BUDGET_USD`.

This is a strong operational brake, not a billing guarantee: usage arrives only
after an LLM response, GLM-5p2's shared rates are placeholders, and a single
in-flight response can cross the remaining estimate. For a strict real-dollar
cap, configure the gateway/account quota at or below the desired amount.

Results are scoped by task so runs cannot overwrite one another:

```text
results/<task>/metrics.json
results/<task>/results.md
results/<task>/out/<harness>.stdout.log
results/<task>/out/<harness>.stderr.log
```

## Capture wire prompts and traces

The complete 2026-07-15 Claude capture—including the configured, `--safe-mode`,
and `--bare` request bodies, with every system block, injected message, tool
description, and JSON Schema—is preserved in the aidocs document
[Claude Code System Prompt](https://aidocs.razorpay.com/app/d/doc_wfmao3n3fn3naruh).

Set `HARNESS_EVAL_CAPTURE=1` to route selected Toroid and Claude API traffic
through loopback recording proxies. A harness-specific flag such as
`HARNESS_EVAL_CAPTURE_CLAUDE=1` or `HARNESS_EVAL_CAPTURE_TOROID=1` also works:

```bash
HARNESS_EVAL_CAPTURE_CLAUDE=1 go run ./examples/harness-eval unicode-truncation claude
```

New exact request and response bodies are written under
`results/<task>/out/<harness>-trace/`, along with request/response metadata. Auth,
API-key, token, and cookie headers are redacted. Trace files use mode `0600`
because request bodies contain the complete system prompt, tool definitions,
conversation history, and potentially repository source.

For a manual or one-turn probe, run the proxy separately and point Claude at
the printed loopback URL:

```bash
go run ./examples/harness-eval proxy "${LLM_GATEWAY_BASE_URL%/v1}" /tmp/claude-trace
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude -p 'Reply with OK' \
  --model glm-5p2 --output-format stream-json --verbose \
  --dangerously-skip-permissions --max-turns 1
```

The proxy forwards bytes without interpreting the Anthropic payload or SSE
response. Keep trace directories out of version control.

Clones and the built toroid binary live in
`os.TempDir()/harness-eval-work`, outside the repository. This is intentional:
an agent in a nested clone can infer and escape into the real working tree.

## Harnesses

| harness | invocation | non-interactive mode |
|---|---|---|
| toroid | working-tree `examples/cli` build | `--run`, NDJSON events |
| claude | `claude -p` | `--output-format stream-json` |
| pi | `pi` | `--mode json`, stdin closed |

Toroid uses `--model llmgateway/glm-5p2`; Claude uses `--model glm-5p2` with
the gateway's Anthropic-compatible endpoint; pi uses its `razorpay` provider
with `--model glm-5p2`. There is no CLI option to change the model.

## Scoring

Every task is scored on the same six grounded outcomes:

1. `branch_pushed` — a new or changed matching branch appeared during this run.
2. `committed` — its tip differs from `master`.
3. `required_changes` — task-declared implementation paths changed.
4. `required_files` — declared delivery artifacts (currently `bench_log.txt`) exist.
5. `builds` — `go build ./...` passes on the pushed branch.
6. `task_tests` — the task's hidden verifier passes after its overlay is added.

The branch check snapshots matching remote refs before starting the agent. This
prevents a failed run from receiving credit for an older successful `-2`/`-3`
branch, which a simple “highest suffix wins” check would do.

## Measurements

Runner-measured, harness-independent metrics include wall and CPU time, peak and
mean process-group RSS, and peak process count. Harness streams provide LLM
turns, tool calls, input/output/cache tokens, and self-reported cost.

For comparison, total cost applies one shared GLM placeholder price table
to each harness's raw usage. GLM-5p2 has no public price in this setup, so the
absolute dollar number is provisional; relative total cost is the useful
measure. Cache accounting still differs somewhat between harness protocols, so
output tokens, turns, tool calls, task success, and elapsed time are the cleanest
comparisons.

## Adding a task

Create a kebab-case `tasks/<id>` directory, add `task.json` and an outcome-only
`prompt.txt`, then add verifier tests under `_verify-overlay/` at the paths they
must occupy in a checkout. Prefer tests that:

- fail on current `master` for the intended reason;
- are deterministic and require no network or model credentials;
- test observable behavior rather than one expected implementation;
- include a regression/boundary case, not merely compilation;
- finish quickly enough that harness overhead remains visible.

Use `go run ./examples/harness-eval list` to validate discovery, then first run
the new task against only one harness and inspect its raw log and pushed diff
before paying for the full comparison.
