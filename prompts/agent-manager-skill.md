---
name: agent-manager
description: Run and monitor coding agents (trk, Codex, Claude, Pi, Opencode) as background async subagents. Start them in background, check their status, read their session transcripts.
---

# Agent Manager

This skill lets you dispatch tasks to external coding agents and monitor
their progress. Use background agents when you want to parallelize work across
multiple harnesses, or when a task benefits from a specific model's strengths.
When launching any agent via the bash tool, pass `background = True` so it runs
as an async subagent.

## Harness table

| Harness | Home env | Default home | Sessions path | Session format |
|---|---|---|---|---|
| trk      | `TOROID_HOME` | `~/.toroid` | `sessions/<id>/` (per-session dirs) | JSON event files |
| Codex    | `CODEX_HOME` | `~/.codex` | `sessions/YYYY/MM/DD/*.jsonl` | JSONL event stream |
| Claude   | `CLAUDE_CONFIG_DIR` | `~/.claude` | `projects/<dir>/<uuid>.jsonl` | JSONL transcript |
| Pi       | `PI_HOME` | `~/.pi` | `agent/sessions/<dir>/*.jsonl` | JSONL |
| Opencode | `OPENCODE_HOME` | `~/.local/share/opencode` | SQLite (`opencode.db`) | `opencode export <id>` |

## Top models

| Harness | Flag | Top model |
|---|---|---|
| trk      | `-model <id> -thinking <level>` | `llmgateway/deepseek-v4-pro` thinking `high` |
| Codex    | `-c model=<id> -c model_reasoning_effort=<level>` | `gpt-5.6-sol` effort `high` |
| Claude   | `--model <id> --effort <level>` | `opus[1m]` effort `high` |
| Pi       | `--model <id>:<thinking>` or `--thinking <level>` | `sonnet:high` |
| Opencode | `-m <provider>/<id>` | `anthropic/claude-sonnet-4-20250514` |
| Open-source (prefer Codex or Pi for deepseek) | via Codex: `-c model=deepseek-v4-pro` or Pi: `--provider deepseek --model deepseek-v4-pro --thinking high` |

Effort/thinking levels: `none | low | high` (trk). `low | medium | high | xhigh | max` (Claude, Pi). Codex uses `low | medium | high`.

## Context window & compaction

| Harness | Context flag | Compaction |
|---|---|---|
| trk      | Config-driven (default 200k) | Auto-compacts at buffer threshold |
| Codex    | Model-dependent (gpt-5.6-sol = 128k) | No explicit flag; auto-compacts |
| Claude   | `--autocompact <auto\|tokens>` e.g. `--autocompact 100k` | Auto or manual token threshold |
| Pi       | Model-dependent | No explicit flag |
| Opencode | Model-dependent | Session-based |

## Starting background agents

### trk
```bash
# One-shot run (--run flag, non-interactive)
trk -model llmgateway/deepseek-v4-pro -thinking high -run 'task description'

# Plain text output only (no event stream)
trk -model llmgateway/deepseek-v4-pro -thinking high -run -plain 'task description'

# Persist to SQLite
trk -model llmgateway/deepseek-v4-pro -thinking high -run -save 'task description'

# Set model via env
export TOROID_MODEL=llmgateway/deepseek-v4-pro
trk -thinking high -run 'task description'
```

### Claude
```bash
# Start background agent (give it a display name; --bg prints the session id
# on stdout, e.g. `backgrounded · 2d5ab496`, that you use to attach/log/stop)
claude --bg --model opus[1m] --effort high -n my-task --cwd /path/to/project 'task description'

# Attach to / read / stop that specific session (no global agent list needed)
claude attach 2d5ab496
claude logs 2d5ab496
claude stop 2d5ab496

# If you must enumerate sessions, scope the query to the project (--cwd) so
# you never tap into Claude's global agent list across unrelated work:
claude agents --json --cwd /path/to/project
# Include completed sessions for this project:
claude agents --json --cwd /path/to/project --all
```

### Codex
```bash
# Non-interactive exec (prints its session id on the first line; capture it)
codex exec -c model=gpt-5.6-sol -c model_reasoning_effort=high \
  --cd /path/to/project 'task description'

# Resume a specific session by id, or the most recent one with --last
codex exec resume <session-id>
codex exec resume --last
```

### Pi
```bash
# Non-interactive mode (-p prints and exits)
pi -p --model sonnet:high --session-dir /path/to/project 'task description'

# Resume a session
pi -r
```

### Opencode
```bash
# Non-interactive run (give the session a name so you can find it later)
opencode run -m anthropic/claude-sonnet-4-20250514 --title 'my-task' 'task description'

# Continue a specific session by id, or the last one
opencode run --session <id> 'follow up message'
opencode run -c 'follow up message'
```

## Reading agent sessions (monitoring)

### trk
```bash
# Sessions are per-session dirs under ~/.toroid/sessions/. Scope by the
# session id you captured on launch, never the whole store:
ls ~/.toroid/sessions/<session-id>/

# Read session events (JSON files in the per-session dir)
ls ~/.toroid/sessions/<session-id>/*.json 2>/dev/null | head -5

# Peek at event content
find ~/.toroid/sessions/<session-id> -name "*.json" -exec cat {} \; | python3 -c "
import sys,json
data = json.load(sys.stdin)
print('kind:', data.get('kind',''))
if 'payload' in data:
    p = data['payload']
    print('payload keys:', list(p.keys())[:5])
" 2>/dev/null

# Query SQLite for a session summary (scoped to the session id you launched)
sqlite3 ~/.toroid/sql.db \
  \"SELECT session_id, SUM(input_tokens), SUM(output_tokens), MAX(timestamp) FROM events WHERE session_id LIKE '<session-id>%' GROUP BY session_id;\"
```

### Claude
```bash
# Read a specific session's transcript by id (from the --bg output / --name)
claude logs <session-id>
claude attach <session-id>

# Read a session transcript (JSONL in project dir)
cat ~/.claude/projects/*/<session-id>.jsonl | head -20

# Get session metadata (status + display name)
cat ~/.claude/sessions/<pid>.json | python3 -c \
  "import sys,json; d=json.load(sys.stdin); print(d['status'], d.get('name',''))"

# Only if you need to enumerate: scope to this project's sessions, never the
# global list
claude agents --json --cwd /path/to/project
```

### Codex
```bash
# Sessions are per-repo JSONL rollouts under ~/.codex/sessions/YYYY/MM/DD.
# Find this repo's sessions by their recorded cwd, then read the matching rollout:
for f in ~/.codex/sessions/*/*/*/rollout-*.jsonl; do
  grep -l '"cwd":"/path/to/project"' "$f" 2>/dev/null
 done
# Resume a specific session by id:
codex exec resume <session-id>

# Read events from a session file (scoped types)
cat <session>.jsonl | python3 -c "
import sys,json
for line in sys.stdin:
    d=json.loads(line.strip())
    t=d.get('type','')
    p=d.get('payload',{})
    # event_msg = turn start/end, agent_msg = actual messages
    if t in ('event_msg','agent_msg'):
        print(t, p.get('type',''), str(p.get('text',''))[:200])
"
```

### Pi
```bash
# Sessions are per-project dirs under ~/.pi/agent/sessions/ (recorded cwd).
# Scope to this project rather than listing everything:
ls ~/.pi/agent/sessions/<project-dir>/

# Read session JSONL
cat ~/.pi/agent/sessions/<project-dir>/*.jsonl | python3 -c "
import sys,json
for line in sys.stdin:
    d=json.loads(line.strip())
    print(d.get('type',''), str(d.get('message',d.get('content','')))[:200])
"
```

### Opencode
```bash
# Sessions record their directory; scope to this project rather than querying
# every session on the machine:
sqlite3 ~/.local/share/opencode/opencode.db \
  "SELECT id, title, time_created FROM session \
   WHERE directory = '/path/to/project' \
   ORDER BY time_created DESC LIMIT 10;"

# Get a session's contents via the supported exporter (no direct SQL needed;
# content lives in JSON blobs across message/part rows)
opencode export <session-id>
```

## Workflow pattern

When asked to run multiple agents:

1. **Start agents** in background using the commands above. Capture the printed session id (e.g. `backgrounded · <id>` for Claude) or assign a name (`-n` for Claude, `--title` for Opencode). Never rely on a PID.
2. **Sleep briefly** (`sleep 30`) then check status.
3. **Monitor** the specific sessions you launched (by id/name via the per-harness commands above, or by reading that session's file). Avoid unscoped global listings (bare `claude agents --json`, un-filtered `codex agents`, `opencode list`, or `ls ~/.toroid/sessions/`), which surface unrelated sessions across the whole machine. Scope to this repo by recorded `cwd`/project.
4. **Report** findings back to the user with a summary.

Prefer trk or Claude for complex multi-step autonomous work (best background agent support).
Use Codex/Pi for single-shot tasks or when a specific model is needed.
Avoid Opencode for background work unless explicitly requested (weaker background support).