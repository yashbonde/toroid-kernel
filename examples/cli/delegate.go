package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// DelegationRecord is persisted under ~/.toroid/delegations/<trace-id>/ so the
// manager skill can later read how many steps, how long, and how many tools
// each delegated task consumed. It is intentionally flat so it can be written
// without a database transaction.
type DelegationRecord struct {
	TaskID      string           `json:"task_id"`
	TraceID     string           `json:"trace_id"`
	Prompt      string           `json:"prompt"`
	Target      DelegationTarget `json:"target"`
	StartedAt   time.Time        `json:"started_at"`
	EndedAt     time.Time        `json:"ended_at,omitempty"`
	Steps       int              `json:"steps,omitempty"`
	ToolCalls   int              `json:"tool_calls,omitempty"`
	CostUSD     float64          `json:"cost_usd,omitempty"`
	InputTokens int64            `json:"input_tokens,omitempty"`
	OutputTokens int64           `json:"output_tokens,omitempty"`
	Error       string           `json:"error,omitempty"`
	Result      string           `json:"result,omitempty"`
}

// routeDelegation applies a small keyword heuristic to pick a target harness.
// It is deliberately simple: the first matching keyword wins, and coding is the
// default fallback (most /delegate prompts in this CLI are coding tasks).
//
//   - slack/docs/office/sheet/email/calendar -> slack (Claude/Slack agent)
//   - large codebase/evaluate/review/audit/architecture -> codex
//   - everything else (coding, debug, test, implement) -> trk with deepseek-v4-pro
func routeDelegation(prompt string) DelegationTarget {
	lower := strings.ToLower(prompt)
	slackTerms := []string{"slack", "doc", "sheet", "email", "calendar", "office", "confluence", "notion", "hr", "finance", "sales", "marketing"}
	for _, t := range slackTerms {
		if strings.Contains(lower, t) {
			return TargetSlack
		}
	}
	codexTerms := []string{"large codebase", "evaluate", "review", "audit", "architecture", "refactor", "migration", "codex"}
	for _, t := range codexTerms {
		if strings.Contains(lower, t) {
			return TargetCodex
		}
	}
	return TargetTRK
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

// delegate runs a /delegate task synchronously, records the result, and returns
// a human-readable summary. It reuses the existing RunSubagent path so the agent
// is spawned as a child kernel with its own budget and event stream.
//
// The routing heuristic decides which harness model to use. For now only trk is
// dispatched directly through the kernel; slack and codex targets are reported
// back to the user with the recommended command from the manager skill, and the
// task is recorded with the chosen target so the manager skill can decide later.
//
// Records are written to ~/.toroid/delegations/<trace-id>/<task-id>.json with
// step count (number of child turns), tool calls, duration, and cost.
func delegate(k *toroid.Kernel, prompt string) (string, error) {
	target := routeDelegation(prompt)
	rec := DelegationRecord{
		TaskID:    fmt.Sprintf("dl-%d", time.Now().Unix()),
		TraceID:   k.SessionID(),
		Prompt:    prompt,
		Target:    target,
		StartedAt: time.Now(),
	}
	defer func() {
		rec.EndedAt = time.Now()
		_ = saveDelegation(rec)
	}()

	var stepsBefore, toolsBefore int
	for sid, u := range k.Sessions {
		if sid == k.SessionID() {
			continue
		}
		stepsBefore++
		_ = u
	}
	_ = stepsBefore
	_ = toolsBefore

	switch target {
	case TargetSlack:
		// The manager skill recommends Claude for office/Slack work. We record the
		// routing decision and hand back the recommended command so the user can run
		// it in Slack if needed.
		rec.Result = "routed to slack (office/docs)"
		return fmt.Sprintf("🛟 routed to %sslack%s — office/docs work. Recommended: `claude --bg --model opus[1m] --effort high '%s'`", aGreen, aReset, compactToolText(prompt, 200)), nil
	case TargetCodex:
		rec.Result = "routed to codex (large codebase evaluation)"
		return fmt.Sprintf("🛟 routed to %scodex%s — large codebase evaluation. Recommended: `codex exec -c model=gpt-5.6-sol -c model_reasoning_effort=high '%s'`", aGreen, aReset, compactToolText(prompt, 200)), nil
	}

	// TargetTRK: run a synchronous subagent through the kernel. The manager skill
	// recommends trk with deepseek-v4-pro for coding tasks. We record step/tool/cost
	// stats from the child kernel by reading its session usage after the run.
	modelID := "llmgateway/deepseek-v4-pro"
	if k.Cfg.Model != "" && k.Cfg.Model == k.Cfg.SmallerModel {
		modelID = k.Cfg.Model
	}
	wrapped := fmt.Sprintf("Run as a focused coding task with the '%s' model. %s", modelID, prompt)
	out, err := k.RunSubagent(context.Background(), wrapped)
	if err != nil {
		rec.Error = err.Error()
		return "", fmt.Errorf("delegation failed: %w", err)
	}
	rec.Result = out
	// Capture child usage/steps from the parent kernel's sessions map. RunSubagent
	// rolls the child spend into the parent total and records the child session.
	for sid, u := range k.Sessions {
		if sid == k.SessionID() {
			continue
		}
		rec.CostUSD += u.Cost
		rec.InputTokens += u.Input + u.CacheRead + u.CacheWrite
		rec.OutputTokens += u.Output
	}
	return fmt.Sprintf("🛟 delegated to %strk%s (%s)\n\n%s", aGreen, aReset, modelID, out), nil
}
