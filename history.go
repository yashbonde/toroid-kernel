package toroid

import (
	"encoding/json"

	"charm.land/fantasy"
)

// ReconstructHistory rebuilds a []fantasy.Message from the events stored in bbolt for a trace.
// Only events after the last compaction are replayed, so the returned history is exactly what
// the kernel would have in memory for a resumed session.
//
// systemPrompt is prepended as a system message when non-empty.
// If spanID is non-empty only events from that span are used; otherwise events from all spans
// under the trace are combined in span order (useful for subagent traces).
func ReconstructHistory(traceID, spanID, systemPrompt string) ([]fantasy.Message, error) {
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
	var compactedBase []fantasy.Message // history produced by the compaction

	if systemPrompt != "" {
		compactedBase = append(compactedBase, fantasy.NewSystemMessage(systemPrompt))
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
		var base []fantasy.Message
		if systemPrompt != "" {
			base = append(base, fantasy.NewSystemMessage(systemPrompt))
		}
		base = append(base, fantasy.NewUserMessage("Tell me the summary of our conversation."))
		summaryMsg := fantasy.NewUserMessage("Here is a summary of our previous interaction for your reference:\n\n" + p.Summary)
		summaryMsg.Role = fantasy.MessageRoleAssistant
		base = append(base, summaryMsg)

		compactedBase = base
		startIdx = i + 1
	}

	history := append([]fantasy.Message{}, compactedBase...)

	// Replay UserPromptSubmit and AssistantTurn events from startIdx.
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
			history = append(history, fantasy.NewUserMessage(p.Prompt))

		case EventAssistantTurn:
			raw, err := json.Marshal(ev.Payload)
			if err != nil {
				continue
			}
			var p AssistantTurnPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				continue
			}
			var msgs []fantasy.Message
			if err := json.Unmarshal(p.Messages, &msgs); err != nil {
				continue
			}
			history = append(history, msgs...)
		}
	}

	return history, nil
}
