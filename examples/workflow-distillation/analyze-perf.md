---
name: Analyze Code Performance
description: Identify performance bottlenecks in a codebase using deterministic analysis
---

## Objective

Analyze a codebase to identify performance hotspots: tight loops, O(n²) algorithms, repeated I/O, and inefficient patterns. Provide actionable recommendations ranked by estimated impact.

## Workflow

Follow these steps in order. Do not skip or reorder.

### Step 1: Enumerate Files

Use the `bash` tool to list the directory structure:

```bash
find <path> -type f -name "*.go" -o -name "*.py" -o -name "*.java" | head -50
```

Identify:
- The main entry point (usually `main.go`, `main.py`, `app.java`)
- The largest files (likely hotspots)
- Package/module structure

### Step 2: Identify Candidate Hotspots

For each of the **top 5 largest files**, read it and look for:

**Nested loops:**
```
for i in range(N):
  for j in range(M):
    # This is O(N*M) — flag it
```

**Repeated I/O in a loop:**
```
for item in items:
  data = read_file(item)  # Called per iteration — N file opens
```

**Repeated compilation or allocation:**
```
for line in lines:
  pattern = re.compile(regex)  # Compiled per iteration
```

**N+1 database queries:**
```
for user_id in user_ids:
  user = db.query(f"SELECT * FROM users WHERE id = {user_id}")  # N queries
```

Document each finding with the file name, line number (approximate), and the pattern.

### Step 3: Estimate Impact

For each hotspot found in Step 2, estimate the impact:

- **O(n²) algorithm**: 10–100x speedup potential (change algorithm or use data structure)
- **Repeated I/O**: 5–20x speedup potential (batch operations)
- **Repeated compilation**: 1.5–3x speedup potential (compile once)
- **N+1 queries**: 5–50x speedup potential (use JOIN or batch)

### Step 4: Cross-Reference with Metrics (Optional)

If the directory contains `profile.txt`, `metrics.json`, or `.pprof` files, read them and see if your analysis matches the profiler's findings. Adjust priority if the profiler shows different hotspots.

### Step 5: Synthesize Report

Return a JSON object with this structure:

```json
{
  "hotspots": [
    {
      "rank": 1,
      "file": "src/processor.go",
      "line": 42,
      "pattern": "nested loop",
      "code_snippet": "for i in range(1000): for j in range(1000): ...",
      "issue": "O(n²) algorithm sorting items twice per outer loop",
      "recommendation": "Use quicksort instead of bubble sort, or pre-sort",
      "estimated_speedup": "50–100x",
      "effort": "low"
    },
    {
      "rank": 2,
      "file": "src/db.go",
      "line": 120,
      "pattern": "N+1 queries",
      "code_snippet": "for user_id in user_ids: SELECT * FROM users WHERE ...",
      "issue": "One database query per user, causing 1000 queries",
      "recommendation": "Use SELECT * FROM users WHERE id IN (...) to batch",
      "estimated_speedup": "20–50x",
      "effort": "low"
    }
  ],
  "total_potential_speedup": "1000–5000x",
  "quick_wins": [
    "Fix O(n²) nested loop — likely 50–100x improvement alone"
  ]
}
```

## Rules

1. **Analyze at most 10 files.** If the codebase is huge, focus on the largest files.
2. **Report only the top 5 hotspots.** Don't enumerate every tiny inefficiency.
3. **Use code snippets, not full function bodies.** Keep context lines under 500 chars.
4. **Return valid JSON.** No markdown, no prose outside the JSON.
5. **Be conservative with estimates.** If unsure, use the lower end of the range.
6. **Stop after 2 minutes.** If you're not done, return what you have.

## Example Input

```
Analyze /home/user/myapp for performance bottlenecks
```

## Example Output

```json
{
  "hotspots": [
    {
      "rank": 1,
      "file": "processor.go",
      "line": 45,
      "pattern": "nested loop",
      "code_snippet": "for i:=0; i<len(items); i++ { for j:=i+1; j<len(items); j++ { ... } }",
      "issue": "Bubble sort re-sorts on every call, O(n²)",
      "recommendation": "Cache sorted result or use quicksort",
      "estimated_speedup": "100x",
      "effort": "low"
    }
  ],
  "total_potential_speedup": "100x",
  "quick_wins": ["Replace bubble sort with quicksort"]
}
```

## Error Handling

If you cannot read a file (permission denied, not found), log a warning but continue analyzing other files.

If the codebase is too large to analyze (>1000 files), focus on the entry point and its direct dependencies.
