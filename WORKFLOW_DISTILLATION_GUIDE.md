# Distilling Recurring Workflows into Deterministic Agents

A practical guide to using toroid traces (collected via a powerful model) to synthesize smaller, cost-effective agents that run on edge models.

## The Problem

A recurring workflow executed by a powerful model (e.g., `llmgateway/glm-4` via Claude) exhibits repeating patterns: tool sequences, decision trees, and common input-output shapes. These patterns are expensive to re-execute on every invocation. The solution is to:

1. **Capture** — Collect complete execution traces on the powerful model
2. **Analyze** — Identify repeating patterns, control flow, and tool sequences
3. **Distill** — Extract minimal prompts + state machines into skills
4. **Deploy** — Run those skills on tiny, fast models (e.g., `gemma-4b` via llmgateway)

---

## Phase 1: Capture Traces with a Powerful Model

### 1.1 Set up tracing infrastructure

```bash
export LLM_GATEWAY_BASE_URL=https://my-gateway.example.com/v1
export LLM_GATEWAY_KEY=sk-...
```

Use the powerful model — e.g., `llmgateway/glm-4` (or similar) — which you've verified has strong tool use, reasoning, and structured output.

### 1.2 Run workflows via the CLI in machine mode

```bash
# One-shot, emit all events as NDJSON
go run ./examples/cli --model llmgateway/glm-4 --thinking high --save \
  --run 'analyze this codebase and identify performance bottlenecks' > trace_001.jsonl

# Repeat for 5–20 representative invocations covering edge cases
for i in {1..20}; do
  go run ./examples/cli --model llmgateway/glm-4 --save \
    --run "$(cat prompts/workflow_$i.txt)" >> traces.jsonl
done
```

### 1.3 Understand the trace structure

Each line in `traces.jsonl` is a `toroid.Event` with:

```json
{
  "kind": "PreToolUse",
  "session_id": "...",
  "trace_id": "...",
  "span_id": "...",
  "emit_ts": 1234567890,
  "seq": 1,
  "payload": {
    "call_id": "call_123",
    "name": "read",
    "args": "{\"filePath\": \"/path/to/file\"}"
  }
}
```

Key event kinds to watch:
- `EventSessionStart` — workflow begins
- `EventPreToolUse` — tool about to be called (includes args)
- `EventPostToolUse` — tool returned successfully (includes result)
- `EventPostToolUseFailure` — tool failed
- `EventAssistantTurn` — LLM's structured output (thinking, text, tool use blocks)
- `EventTurnCost` — cost of the turn
- `EventPreCompact` / `EventPostCompact` — history was summarized
- `EventStop` / `EventSessionEnd` — workflow ended

---

## Phase 2: Analyze Traces

### 2.1 Extract tool call sequences

Write a simple parser to build a call graph:

```python
import json
from collections import defaultdict, Counter

patterns = []
with open('traces.jsonl') as f:
    session_calls = []
    for line in f:
        ev = json.loads(line)
        if ev['kind'] == 'PreToolUse':
            session_calls.append(ev['payload']['name'])
        elif ev['kind'] == 'SessionEnd':
            patterns.append(session_calls)
            session_calls = []

# Find the most common subsequences
call_freqs = Counter()
for pattern in patterns:
    for i in range(len(pattern)):
        for j in range(i+1, min(i+5, len(pattern)+1)):
            call_freqs[tuple(pattern[i:j])] += 1

print("Top 10 tool subsequences:")
for seq, count in call_freqs.most_common(10):
    print(f"  {' -> '.join(seq)}: {count} times")
```

### 2.2 Identify decision points

Look for patterns where tool results diverge:

```python
# Extract decision trees: "if result contains X, call Y; else call Z"
decisions = []
with open('traces.jsonl') as f:
    prev_result = None
    for line in f:
        ev = json.loads(line)
        if ev['kind'] == 'PostToolUse':
            prev_result = ev['payload']['result']
        elif ev['kind'] == 'PreToolUse' and prev_result:
            decisions.append({
                'prev_result_snippet': prev_result[:100],
                'next_tool': ev['payload']['name'],
                'args': ev['payload']['args']
            })
```

### 2.3 Identify parameter patterns

For each tool, collect example argument values:

```python
tool_args = defaultdict(list)
with open('traces.jsonl') as f:
    for line in f:
        ev = json.loads(line)
        if ev['kind'] == 'PreToolUse':
            tool_name = ev['payload']['name']
            args = json.loads(ev['payload']['args'])
            tool_args[tool_name].append(args)

for tool, arg_list in sorted(tool_args.items()):
    print(f"\n{tool}:")
    print(f"  Called {len(arg_list)} times")
    if arg_list:
        print(f"  Example args: {arg_list[0]}")
```

---

## Phase 3: Distill Into Skills

A **skill** is a Markdown file with:
- Frontmatter: `name` and `description` (auto-loaded into the system prompt)
- Body: detailed instructions, examples, and state machines (loaded on-demand via the `skill` tool)

### 3.1 Create a skill from traced patterns

**File: `~/.toroid/skills/analyze-bottlenecks.md`**

```markdown
---
name: Analyze Performance Bottlenecks
description: Identify slow functions and I/O in a codebase
---

## Workflow

This skill follows a deterministic pattern:

1. **Read the codebase**
   - List top-level files and dirs
   - Identify main entry points and hot paths (based on naming)
   
2. **Profile the hottest functions**
   - For each candidate:
     - Read its source (full body)
     - Identify nested loops, I/O calls, expensive operations
   - Track findings in a structured list
   
3. **Cross-reference with metrics (if available)**
   - If there's a profile.txt or metrics.json, read and correlate
   
4. **Synthesize**
   - Rank findings by estimated impact
   - Suggest fixes (caching, batching, algorithm changes)

## Decision Rules

- If you see a loop calling a function inside it, that's O(n²) — flag it
- If you see repeated file I/O in a loop, flag for batching
- If you see regex compilation in a loop, suggest pre-compiling

## Example Invocations

### Input
```
Analyze the performance bottlenecks in /home/user/myapp
```

### Expected output
```
1. Main hotspot: src/process.go line 45 — nested loop iterating 1000×1000
   - Recommendation: Use a hash set instead
   - Estimated speedup: 10–100x

2. Secondary hotspot: src/db.go line 120 — N+1 queries in a loop
   - Recommendation: Batch queries with JOIN
   - Estimated speedup: 5–20x

3. Minor: Regex compiled on every invocation
   - Recommendation: Compile once at init
   - Estimated speedup: 1.5–2x
```

## Assumptions

- Source code is readable and under 1 MiB per function
- No proprietary / binary analysis needed
- Function names are reasonably descriptive
```

### 3.2 Keep the skill deterministic

The skill should reduce the model's degrees of freedom:

- **Specify exact tool sequences** — "always read files before analyzing"
- **Enumerate decision rules** — "if X is true, do Y; else do Z"
- **Provide examples** — input/output pairs that establish the expected format
- **Set boundaries** — "analyze at most 10 functions" to control cost

### 3.3 Reduce prompt size for edge models

A skill for a 4B model should be **concise but complete**:

```markdown
---
name: Summarize Logs
description: Extract errors and warnings from log files
---

## Steps

1. Read the log file
2. Filter lines containing ERROR or WARN
3. Group by message prefix (first 50 chars)
4. Count occurrences
5. Return JSON: {errors: [{msg, count}], warnings: [{msg, count}]}

## Example

Input: `/var/log/app.log`

Output:
```json
{
  "errors": [
    {"msg": "Connection timeout", "count": 5},
    {"msg": "Out of memory", "count": 2}
  ],
  "warnings": [
    {"msg": "Deprecated API", "count": 12}
  ]
}
```

## Rules

- Stop after 100 lines analyzed if file is huge
- Merge similar messages (Levenshtein distance < 10)
- Return counts, not full messages (to save tokens)
```

---

## Phase 4: Validate Skills on Edge Models

### 4.1 Test on the target model

```bash
export TOROID_MODEL=llmgateway/gemma-4b
go run ./examples/cli --run 'analyze /path/to/code for bottlenecks'
```

### 4.2 Compare outputs

| Aspect | Powerful Model | Edge Model | Action |
|--------|---|---|---|
| **Correctness** | Found 5 real issues | Found 3 real, 1 false positive | Refine skill decision rules |
| **Format** | Valid JSON | Malformed JSON | Add schema constraint to skill |
| **Completeness** | 10 functions analyzed | 3 functions analyzed | Reduce max functions in skill |
| **Cost** | $0.05 | $0.001 | ✓ Use edge model |
| **Latency** | 8s | 0.5s | ✓ Use edge model |

### 4.3 A/B test at runtime

Route a small percentage of requests to each model and measure:
- Correctness (do human reviewers accept the output?)
- Cost reduction
- Latency improvement

Once the edge model is >95% accurate, fully migrate.

---

## Phase 5: Deploy with Structured Output

### 5.1 Add schema enforcement

Toroid supports structured output via forced tool calls. For skills, define a schema:

```go
// In your skill invocation code:
k, _ := toroid.NewKernel(ctx, toroid.Config{
    Model: "llmgateway/gemma-4b",
    // ...
})

// Define the output schema as a JSON Schema
schema := map[string]interface{}{
    "type": "object",
    "properties": map[string]interface{}{
        "bottlenecks": map[string]interface{}{
            "type": "array",
            "items": map[string]interface{}{
                "type": "object",
                "properties": map[string]interface{}{
                    "file": map[string]interface{}{"type": "string"},
                    "line": map[string]interface{}{"type": "integer"},
                    "issue": map[string]interface{}{"type": "string"},
                    "recommendation": map[string]interface{}{"type": "string"},
                },
                "required": []string{"file", "issue"},
            },
        },
    },
    "required": []string{"bottlenecks"},
}

// Run with schema constraint
out, _, err := k.RunWithSchema(ctx, prompt, schema)
```

### 5.2 Persist skill invocations

Log the skill name and invocation in `~/.toroid/sessions/<session>/transcript.jsonl`:

```json
{"kind": "TraceLog", "type": "info", "message": "Invoked skill: analyze-bottlenecks"}
{"kind": "UserPromptSubmit", "prompt": "analyze /path/to/code"}
...
{"kind": "TraceLog", "type": "info", "message": "Skill complete: 3 bottlenecks identified"}
```

---

## Phase 6: Multi-Step Workflows (Subagents)

For complex workflows that can't be distilled into a single skill, use **subagents**:

### 6.1 Create a parent agent

```go
k, _ := toroid.NewKernel(ctx, toroid.Config{
    Model: "llmgateway/gemma-4b",
    // ...
})

// The parent orchestrates: "first analyze, then optimize, then test"
out, _, err := k.Run(ctx, `
Use the following workflow:
1. Run skill: analyze-bottlenecks
2. For each bottleneck, run skill: suggest-optimization
3. For each suggestion, run skill: estimate-improvement
4. Synthesize results into a prioritized list
`)
```

### 6.2 Skills call subagents

A skill's Markdown body can invoke subagents:

```markdown
---
name: Orchestrate Code Review
description: Run a multi-stage code review
---

## Workflow

1. Run a subagent: `subagent("Analyze for security issues")`
2. Run a subagent: `subagent("Check for performance problems")`
3. Run a subagent: `subagent("Verify test coverage")`
4. Synthesize the three reports into one review document

## Expected Output

JSON with:
- security_issues: [...]
- performance_issues: [...]
- coverage_gaps: [...]
- overall_grade: "A" | "B" | "C" | "D" | "F"
```

---

## Phase 7: Monitoring & Iteration

### 7.1 Track skill accuracy

Collect a ground-truth dataset of 50–100 invocations. For each, record:
- Skill output
- Human-reviewed "correct" output
- Match score (e.g., Jaccard similarity on extracted entities)

```python
matches = 0
for invocation in ground_truth:
    skill_output = invoke_skill(invocation['input'])
    correct_output = invocation['expected']
    if accuracy(skill_output, correct_output) > 0.9:
        matches += 1
print(f"Accuracy: {matches}/{len(ground_truth)} ({100*matches/len(ground_truth):.1f}%)")
```

### 7.2 Log failures and edge cases

When a skill fails or produces an error:

```go
k.On(toroid.EventPostToolUseFailure, func(ctx context.Context, e toroid.Event) error {
    // Log the failure for later analysis
    log.Printf("Skill error: %s | Input: %s | Error: %s",
        e.Payload.(*toroid.ToolUseResultPayload).Name,
        inputData, // captured from context
        e.Payload.(*toroid.ToolUseResultPayload).Error)
    return nil
})
```

### 7.3 Continuously refine

Every month, re-run Phase 2 (Analyze) on the newest traces. If you spot new patterns or decision rules, update the skill in Phase 3. A/B test the new version on the edge model.

---

## Reference Implementation: Quick Start

### Step 1: Collect traces

```bash
mkdir -p traces
for i in {1..10}; do
  go run ./examples/cli --model llmgateway/glm-4 --save --tokens \
    --run "$(cat prompts/$i.txt)" >> traces/trace_$i.jsonl
done
```

### Step 2: Analyze patterns

```bash
# Run the analysis script from Phase 2
python3 analyze_traces.py traces/
```

### Step 3: Create a skill

```bash
mkdir -p ~/.toroid/skills
cat > ~/.toroid/skills/my-task.md << 'EOF'
---
name: My Task
description: Brief description
---

## Steps
1. Do X
2. Do Y
3. Return Z
EOF
```

### Step 4: Test on edge model

```bash
TOROID_MODEL=llmgateway/gemma-4b \
go run ./examples/cli --run "test prompt"
```

### Step 5: Measure

```bash
# Cost before (powerful model)
cost_before=$(curl -s https://gateway/api/costs | jq '.total')

# Cost after (edge model)
cost_after=$(curl -s https://gateway/api/costs | jq '.total')

echo "Cost reduction: $(( (cost_before - cost_after) * 100 / cost_before ))%"
```

---

## Key Takeaways

1. **Traces are the spec** — Run the workflow on a powerful model once, capture every tool call and decision, then distill that into a repeatable skill.

2. **Skills are prompts + rules** — A skill isn't machine learning; it's a deterministic specification written as Markdown that a smaller model can follow reliably.

3. **Schema enforcement is essential** — For edge models, always enforce JSON schemas via `RunWithSchema` to ensure consistent, parseable output.

4. **Test incrementally** — A/B test on a subset before full migration.

5. **Iterate constantly** — Every few weeks, collect new traces and look for patterns you missed. Update the skill to handle them.

This approach trades a one-time investment in tracing and distillation for dramatic cost and latency improvements across all future invocations.
