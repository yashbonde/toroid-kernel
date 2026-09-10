package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

// DelegationTarget is the harness chosen by the heuristic router for a /delegate
// task. It mirrors the harness table in prompts/agent-manager-skill.md.
type DelegationTarget string

const (
	TargetSlack DelegationTarget = "slack"
	TargetCodex DelegationTarget = "codex"
	TargetTRK   DelegationTarget = "trk"
)

// DelegationStatus is where a task is in its life. A record is written as soon
// as the task starts, so a delegation that is still running — or one whose
// parent died mid-flight — is distinguishable from one that finished.
type DelegationStatus string

const (
	StatusRunning   DelegationStatus = "running"
	StatusSucceeded DelegationStatus = "succeeded"
	StatusFailed    DelegationStatus = "failed"
	StatusRouted    DelegationStatus = "routed" // handed to an external harness
)

// delegationSeq disambiguates tasks started within the same second.
var delegationSeq atomic.Uint64

// DelegationRecord is persisted under ~/.toroid/delegations/<trace-id>/ so the
// manager skill can later read how many steps, how long, and how many tools
// each delegated task consumed. It is intentionally flat so it can be written
// without a database transaction.
type DelegationRecord struct {
	TaskID  string           `json:"task_id"`
	TraceID string           `json:"trace_id"`
	Prompt  string           `json:"prompt"`
	Target  DelegationTarget `json:"target"`
	Status  DelegationStatus `json:"status"`
	// SessionID is the subagent's own session; ParentSessionID is the session
	// that delegated it. Together they reconstruct the delegation tree, and they
	// match the session_id / parent_session_id on the child's LLM requests.
	SessionID       string `json:"session_id,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// Model is what actually ran. RequestedModel is set only when the caller
	// named one, and FellBack marks that it did not work and the default ran.
	Model          string    `json:"model,omitempty"`
	RequestedModel string    `json:"requested_model,omitempty"`
	FellBack       bool      `json:"fell_back,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at,omitempty"`
	Steps          int       `json:"steps,omitempty"`
	ToolCalls      int       `json:"tool_calls,omitempty"`
	CostUSD        float64   `json:"cost_usd,omitempty"`
	InputTokens    int64     `json:"input_tokens,omitempty"`
	OutputTokens   int64     `json:"output_tokens,omitempty"`
	Error          string    `json:"error,omitempty"`
	Result         string    `json:"result,omitempty"`
}

// routeDelegation applies a small keyword heuristic to pick a target harness.
// Terms match on word boundaries: a substring scan sends "fix the docker build"
// to Slack on "doc" and "walk the thread" on "hr". Coding is the default, since
// most /delegate prompts in this CLI are coding tasks.
//
//   - slack/docs/office/sheet/email/calendar -> slack (Claude/Slack agent)
//   - large codebase/evaluate/review/audit/architecture -> codex
//   - everything else (coding, debug, test, implement) -> trk
func routeDelegation(prompt string) DelegationTarget {
	words := delegationWords(prompt)
	slackTerms := []string{"slack", "doc", "docs", "sheet", "sheets", "email", "calendar", "office", "confluence", "notion", "hr", "finance", "sales", "marketing"}
	if containsAnyWord(words, slackTerms) {
		return TargetSlack
	}
	codexTerms := []string{"evaluate", "review", "audit", "architecture", "refactor", "migration", "codex"}
	if strings.Contains(strings.ToLower(prompt), "large codebase") || containsAnyWord(words, codexTerms) {
		return TargetCodex
	}
	return TargetTRK
}

// delegationWords lowercases prompt and splits it into words, so terms match
// whole words rather than any substring.
func delegationWords(prompt string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(prompt), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		out[w] = true
	}
	return out
}

func containsAnyWord(words map[string]bool, terms []string) bool {
	for _, t := range terms {
		if words[t] {
			return true
		}
	}
	return false
}

// parseDelegateArgs splits a /delegate argument string into an explicit model
// override and the task itself. The model is named with a leading flag —
// "/delegate --model <id> <task>" (or "-m <id>") — so the user can pick a model
// without the router having to guess one out of prose. Anything else is task text.
func parseDelegateArgs(args string) (prompt, model string) {
	fields := strings.Fields(args)
	for len(fields) >= 2 {
		switch fields[0] {
		case "--model", "-model", "-m":
			model = fields[1]
			fields = fields[2:]
			continue
		}
		break
	}
	return strings.Join(fields, " "), model
}

// delegationDir returns ~/.toroid/delegations/<trace-id>, creating it if needed.
func delegationDir(traceID string) (string, error) {
	home, err := toroid.ToroidHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "delegations", traceID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// saveDelegation writes a DelegationRecord to ~/.toroid/delegations/<trace-id>/<task-id>.json.
func saveDelegation(rec DelegationRecord) error {
	dir, err := delegationDir(rec.TraceID)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, rec.TaskID+".json")
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

// newTaskID is unique per task: a bare Unix second collides for two
// delegations started in the same second, and the second overwrites the first.
func newTaskID() string {
	return fmt.Sprintf("dl-%d-%d", time.Now().Unix(), delegationSeq.Add(1))
}

// delegate dispatches a /delegate task and returns immediately. Coding work goes
// to a background subagent through the kernel, so the caller keeps working while
// it runs; its result is delivered later through the kernel's message queue,
// waking the parent if it has gone idle.
//
// model is the model the user explicitly asked for, or empty to use the default.
// An override that fails to run falls back to the default inside the kernel.
//
// A record is written to ~/.toroid/delegations/<trace-id>/<task-id>.json at
// start with status "running", and rewritten when the task finishes with its
// session ids, step and tool counts, duration, and cost.
func delegate(k *toroid.Kernel, prompt, model string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("nothing to delegate: give /delegate a task")
	}
	target := routeDelegation(prompt)
	rec := DelegationRecord{
		TaskID:          newTaskID(),
		TraceID:         k.TraceID(),
		ParentSessionID: k.SessionID(),
		Prompt:          prompt,
		Target:          target,
		RequestedModel:  strings.TrimSpace(model),
		StartedAt:       time.Now(),
	}

	switch target {
	case TargetSlack:
		// The manager skill recommends Claude for office/Slack work. We record the
		// routing decision and hand back the recommended command so the user can run
		// it in Slack if needed.
		rec.Status, rec.EndedAt = StatusRouted, time.Now()
		rec.Result = "routed to slack (office/docs)"
		if err := saveDelegation(rec); err != nil {
			return "", fmt.Errorf("could not record delegation: %w", err)
		}
		return fmt.Sprintf("🛟 routed to %sslack%s — office/docs work. Recommended: `claude --bg --model opus[1m] --effort high '%s'`", aGreen, aReset, compactToolText(prompt, 200)), nil
	case TargetCodex:
		rec.Status, rec.EndedAt = StatusRouted, time.Now()
		rec.Result = "routed to codex (large codebase evaluation)"
		if err := saveDelegation(rec); err != nil {
			return "", fmt.Errorf("could not record delegation: %w", err)
		}
		return fmt.Sprintf("🛟 routed to %scodex%s — large codebase evaluation. Recommended: `codex exec -c model=gpt-5.6-sol -c model_reasoning_effort=high '%s'`", aGreen, aReset, compactToolText(prompt, 200)), nil
	}

	// TargetTRK: run through the kernel as a background subagent. The record is
	// completed from the run's own result rather than by reading k.Sessions,
	// which accumulates every subagent the session has ever run and would charge
	// this task for all of them.
	rec.Status = StatusRunning
	// Fail before spawning: a task whose record cannot be written (read-only or
	// full home) would otherwise report as delegated while leaving no trace of
	// itself for the manager skill to find.
	if err := saveDelegation(rec); err != nil {
		return "", fmt.Errorf("could not record delegation: %w", err)
	}

	// The callback owns its own copy of the record: it runs on the background
	// goroutine, and sharing the caller's would be a data race.
	final := rec
	taskID := k.SpawnBackgroundWith(prompt, toroid.SubagentOpts{Model: model, TaskID: rec.TaskID}, func(res toroid.SubagentResult, err error) {
		rec := final
		rec.EndedAt = time.Now()
		rec.SessionID = res.SessionID
		rec.Model = res.Model
		rec.FellBack = res.FellBack
		rec.Steps = res.Steps
		rec.ToolCalls = res.ToolCalls
		rec.CostUSD = res.Usage.Cost
		rec.InputTokens = res.Usage.Input + res.Usage.CacheRead + res.Usage.CacheWrite
		rec.OutputTokens = res.Usage.Output
		if err != nil {
			rec.Status, rec.Error = StatusFailed, err.Error()
		} else {
			rec.Status, rec.Result = StatusSucceeded, res.Output
		}
		if err := saveDelegation(rec); err != nil {
			// The task itself finished; only its record is lost. Say so rather
			// than leaving it marked "running" forever with no explanation.
			k.Enqueue(fmt.Sprintf("[delegation %s finished but its record could not be written: %v]", rec.TaskID, err))
		}
	})

	shown := "default model"
	if m := strings.TrimSpace(model); m != "" {
		shown = m
	}
	return fmt.Sprintf("🛟 delegated to %strk%s in the background (%s) — task %s. You'll be notified when it finishes.", aGreen, aReset, shown, taskID), nil
}
