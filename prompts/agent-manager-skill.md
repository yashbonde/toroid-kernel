---
name: agent-manager
description: Run and monitor coding agents (trk, Codex, Claude, Pi, Opencode) as background async subagents. Start them in background, check their status, read their session transcripts.
---

# Agent Manager

This skill lets you dispatch tasks to external coding agents and monitor
their progress. Use background agents when you want to parallelize work across
multiple harnesses, or when a task benefits from a specific model's strengths.

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
# Start background agent
claude --bg --model opus[1m] --effort high --cwd /path/to/project 'task description'

# List running agents
claude agents --json

# List all (including completed)
claude agents --json --all
```

### Codex
```bash
# Non-interactive exec
codex exec -c model=gpt-5.6-sol -c model_reasoning_effort=high \
  --cd /path/to/project 'task description'

# Resume last session
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
# Non-interactive run
opencode run 'task description' -m anthropic/claude-sonnet-4-20250514

# Continue last session
opencode run -c 'follow up message'
```

## Reading agent sessions (monitoring)

### trk
```bash
# List all sessions
ls ~/.toroid/sessions/

# Read session events (JSON files in per-session dirs)
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

# Query SQLite for session summary
sqlite3 ~/.toroid/sql.db \
  \"SELECT session_id, SUM(input_tokens), SUM(output_tokens), MAX(timestamp) FROM events WHERE session_id LIKE '<session-id>%' GROUP BY session_id;\"
```

### Claude
```bash
# List active agents with status
claude agents --json

# Read a session transcript (JSONL in project dir)
cat ~/.claude/projects/*/<session-id>.jsonl | head -20

# Get session metadata
cat ~/.claude/sessions/<pid>.json | python3 -c \
  "import sys,json; d=json.load(sys.stdin); print(d['status'], d.get('name',''))"
```

### Codex
```bash
# Find latest session
ls -t ~/.codex/sessions/2026/*/*/*.jsonl | head -1

# Read events (scoped types)
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
# List sessions
ls ~/.pi/agent/sessions/

# Read session JSONL
cat ~/.pi/agent/sessions/<dir>/*.jsonl | python3 -c "
import sys,json
for line in sys.stdin:
    d=json.loads(line.strip())
    print(d.get('type',''), str(d.get('message',d.get('content','')))[:200])
"
```

### Opencode
```bash
# List sessions from SQLite
sqlite3 ~/.local/share/opencode/opencode.db \
  "SELECT id, model, created_at FROM session ORDER BY created_at DESC LIMIT 10;"

# Get messages for a session
sqlite3 ~/.local/share/opencode/opencode.db \
  "SELECT role, substr(content,1,200) FROM session_message WHERE session_id='<id>' ORDER BY created_at;"

# Export full session as JSON
opencode export <session-id>
```

## Workflow pattern

When asked to run multiple agents:

1. **Start agents** in background using the commands above. Use `&` and capture PID.
2. **Sleep briefly** (`sleep 30`) then check status.
3. **Monitor** by reading session files or running the list commands.
4. **Report** findings back to the user with a summary.

Prefer trk or Claude for complex multi-step autonomous work (best background agent support).
Use Codex/Pi for single-shot tasks or when a specific model is needed.
Avoid Opencode for background work unless explicitly requested (weaker background support).