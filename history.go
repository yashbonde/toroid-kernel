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
	td, err := LoadTraceData(traceID)
	if err != nil {
		return nil, err
	}

	// Collect events for the requested span (or all spans if spanID is empty).
	var events []Event
	for _, span := range td.Spans {
		if spanID == "" || span.SpanID == spanID {
			events = append(events, span.Events...)
		}
	}
	if len(events) == 0 {
		return nil, nil
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
			if err := json.Unmarshal(raw, &p); err != nil || p.Prompt == "" {
				continue
			}
			// Replay reconstructs history from persisted prompts; media caps and
			// capability were already enforced when the prompt was first submitted,
			// so warnings here are not re-surfaced.
			msg, _, _ := parseUserMessage(p.Prompt, workDir, ResolveModel(model))
			history = append(history, msg)

		case EventTurnCompleted:
			raw, err := json.Marshal(ev.Payload)
			if err != nil {
				continue
			}
			var p TurnPayload
			if err := json.Unmarshal(raw, &p); err != nil || len(p.Content) == 0 {
				continue
			}
			var msgs []llm.Message
			if err := json.Unmarshal(p.Content, &msgs); err != nil {
				continue
			}
			history = append(history, msgs...)
		}
	}

	return history, nil
}
