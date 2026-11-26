package toroid

import (
	"encoding/json"

	"github.com/yashbonde/toroid-kernel/llm"
)

// ReconstructHistory rebuilds a []llm.Message from the events stored for a trace.
// Only events after the last compaction are replayed, so the returned history is exactly what
// the kernel would have in memory for a resumed session.
//
// systemPrompt is prepended as a system message when non-empty. Prefer leaving
// it empty: the live kernel sends system as the single leading system message on
// every llm-step, so History should not also carry one (double bill + cache bust).
// If spanID is non-empty only events from that span are used; otherwise events from all spans
// under the trace are combined in span order (useful for subagent traces).
// workDir resolves any relative image refs in stored prompts (persisted refs are
// already ~-rooted, so it normally only matters for legacy data). model gates
// image inlining on replay via the catalog's capability check (M8).
func ReconstructHistory(traceID, spanID, systemPrompt, workDir, model string) ([]llm.Message, error) {
	var events []Event
	if spanID != "" {
		db, err := openDefaultSQL()
		if err != nil {
			return nil, err
		}
		events, err = loadSpanEvents(db, traceID, spanID)
		if err != nil {
			return nil, err
		}
	} else {
		td, err := LoadTraceData(traceID)
		if err != nil {
			return nil, err
		}
		for _, span := range td.Spans {
			events = append(events, span.Events...)
		}
	}
	return replayHistory(events, systemPrompt, workDir, model), nil
}

func replayHistory(events []Event, systemPrompt, workDir, model string) []llm.Message {
	if len(events) == 0 {
		return nil
	}

	// Find the index just after the last EventPostCompact (i.e. the compacted baseline).
	// If there was no compaction, startIdx = 0.
	startIdx := 0
	var compactedBase []llm.Message // history produced by the compaction

	if systemPrompt != "" {
		compactedBase = append(compactedBase, llm.NewSystemMessage(systemPrompt))
	}

	for i, ev := range events {
		if ev.Kind != EventPostCompact {
			continue
		}
		// Unmarshal the summary.
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			continue
		}
		var p CompactSummaryPayload
		if err := json.Unmarshal(raw, &p); err != nil || p.Summary == "" {
			continue
		}
		// Build the compacted baseline: [system?, user-ask, assistant-summary]
		var base []llm.Message
		if systemPrompt != "" {
			base = append(base, llm.NewSystemMessage(systemPrompt))
		}
		base = append(base, llm.NewUserMessage("Tell me the summary of our conversation."))
		summaryMsg := llm.NewUserMessage("Here is a summary of our previous interaction for your reference:\n\n" + p.Summary)
		summaryMsg.Role = llm.RoleAssistant
		base = append(base, summaryMsg)

		compactedBase = base
		startIdx = i + 1
	}

	history := append([]llm.Message{}, compactedBase...)

	// Replay UserPromptSubmit and TurnCompleted events from startIdx.
	// TurnCompleted.Content carries the turn's full structured content — what
	// EventAssistantTurn used to carry as its own event kind.
	for _, ev := range events[startIdx:] {
		switch ev.Kind {
		case EventUserPromptSubmit:
			raw, err := json.Marshal(ev.Payload)
			if err != nil {
				continue
			}
			var p UserPromptPayload
			if err := json.Unmarshal(raw, &p); err != nil || (p.Prompt == "" && len(p.Attachments) == 0) {
				continue
			}
			// Replay reconstructs history from persisted prompts; media caps and
			// capability were already enforced when the prompt was first submitted,
			// so warnings here are not re-surfaced.
			msg, _, _ := parseUserMessage(p.Prompt, workDir, ResolveModel(model))
			// If an image moved or the new model is text-only, retain its path.
			_ = appendAttachments(&msg, p.Attachments, workDir, ResolveModel(model))
			history = append(history, msg)

		case EventTurnCompleted, EventKind("AssistantTurn"):
			raw, err := json.Marshal(ev.Payload)
			if err != nil {
				continue
			}
			var p struct {
				Content  json.RawMessage `json:"content"`
				Messages json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				continue
			}
			if ev.Kind == "AssistantTurn" {
				p.Content = p.Messages
			}
			var msgs []llm.Message
			if err := json.Unmarshal(p.Content, &msgs); err != nil {
				continue
			}
			history = append(history, msgs...)
		}
	}

	return history
}

// restoreSpan continues the same persistent event sequence, conversation and
// accounting after a restart. A post-compaction window takes precedence over
// the larger measured window from the step before it.
func (k *Kernel) restoreSpan(store *Store, cfg Config) (bool, error) {
	events, err := loadSpanEvents(store.DB(), cfg.TraceID, cfg.SessionID)
	if err != nil || len(events) == 0 {
		return false, err
	}
	k.seq.Store(events[len(events)-1].Seq)
	k.History = replayHistory(events, "", cfg.WorkDir, cfg.Model)
	k.runningCostUSD = store.LoadLastTotalUSD(cfg.TraceID, cfg.SessionID)
	for _, e := range events {
		payload, _ := e.Payload.(map[string]any)
		switch e.Kind {
		case EventPostCompact:
			if n, ok := payload["tokens_after"].(float64); ok {
				k.currentTokens = int(n)
			}
		case EventTurnCompleted:
			if raw := payload["turn_usage"]; raw != nil {
				var u Usage
				b, _ := json.Marshal(raw)
				if json.Unmarshal(b, &u) == nil {
					k.currentTokens = int(u.Input + u.Output + u.CacheRead + u.CacheWrite)
				}
			}
		case EventStop:
			var counts map[string]Usage
			b, _ := json.Marshal(payload["tokens"])
			if json.Unmarshal(b, &counts) == nil && counts != nil {
				k.Sessions = counts
			}
		}
	}
	return true, nil
}
