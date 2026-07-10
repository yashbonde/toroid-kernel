package toroid

import (
	"context"
	"encoding/json"
	"io"

	"charm.land/fantasy"
)

// Kernel-owned tool loop (Phase B, P1.1). This drives each turn's LLM call
// through the Step layer — one product llm-step per turn — with the kernel
// executing tools between calls. It is the architecture the port prefers (§2):
// the Kernel owns turns/tools/compaction; a Step performs a single LLM call.
//
// It reproduces the guarantees of the Fantasy Agent.Stream path: MaxIter cap,
// repeat-call loop guard, queue-interrupt at turn boundaries, mid-turn
// compaction under context pressure, tool events, and per-turn cost. It is
// opt-in (Config.UseStepLoop) so the proven Agent path stays the production
// default until this is validated against a live gateway.
//
// v1 uses Step.Complete (non-streaming): the assistant text is written after
// each turn rather than token-by-token, and live reasoning deltas are not
// emitted. Everything else — tools, history shape, cost, guards — matches.
func (k *Kernel) streamViaStep(ctx context.Context, w io.Writer) error {
	model := ResolveModel(k.Cfg.Model)
	tools := k.agentTools()

	var recentSigs []string // rolling window of tool-call signatures for the loop guard
	turns := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		res, err := k.Step.Complete(ctx, model, Context{
			System:   k.SystemPrompt,
			Messages: k.History,
			Tools:    tools,
		}, k.stepOptions())
		if err != nil {
			return err
		}
		turns++

		// Record the assistant turn: append to history, bill, update occupancy.
		assistant := assistantMessageFromContent(res.Content)
		k.appendStepMessages(ctx, []fantasy.Message{assistant})
		k.recordUsage(ctx, res.Usage)
		k.setWindowTokens(res.Usage)

		toolCalls := res.Content.ToolCalls()

		// Write assistant text once the turn's tool intent is known: a final turn
		// (no tools) is the user-visible answer.
		if len(toolCalls) == 0 {
			if _, err := io.WriteString(w, res.Content.Text()); err != nil {
				return err
			}
			return nil // end of chat
		}

		// Execute tools locally (not an llm-step) and append their results.
		toolMsg, sig := k.runToolCalls(ctx, tools, toolCalls)
		k.appendStepMessages(ctx, []fantasy.Message{toolMsg})

		// Loop guard: stop if the last MaxRepeatCalls turns made the identical
		// tool call(s) with identical result(s) — a spin making no progress.
		if n := k.Cfg.MaxRepeatCalls; n > 1 {
			recentSigs = append(recentSigs, sig)
			if len(recentSigs) > n {
				recentSigs = recentSigs[len(recentSigs)-n:]
			}
			if allEqual(recentSigs, n) {
				k.Logf("loop guard: stopping after %d consecutive identical tool calls with identical results", n)
				return nil
			}
		}

		// MaxIter cap on tool-call turns.
		if k.Cfg.MaxIter > 0 && turns >= k.Cfg.MaxIter {
			k.Logf("step loop: reached MaxIter=%d", k.Cfg.MaxIter)
			return nil
		}

		// Queue interrupt at this safe turn boundary: inject queued messages and
		// continue the same chat on the extended history.
		if queued := k.drainQueue(); len(queued) > 0 {
			for _, qm := range queued {
				k.History = append(k.History, fantasy.NewUserMessage(qm))
			}
			_ = k.Fire(ctx, string(EventQueueInterrupt), &QueueInterruptPayload{Messages: queued})
			continue
		}

		// Context pressure: compact between llm-steps (M4) and continue.
		if k.overContextThreshold() {
			if err := k.Compact(ctx); err != nil {
				return err
			}
		}
	}
}

// agentTools returns the kernel's registered tools as Fantasy AgentTools for a
// Step Context (same set the Fantasy Agent path receives via WithTools).
func (k *Kernel) agentTools() []fantasy.AgentTool {
	if k.Tools == nil {
		return nil
	}
	var out []fantasy.AgentTool
	for _, t := range k.Tools.Tools() {
		out = append(out, t.AgentTool)
	}
	return out
}

// appendStepMessages appends a turn's messages to history, index-aligning
// StepHistoryStart so pruneOldToolCalls keeps working, and fires
// EventAssistantTurn so history can be reconstructed from events alone.
func (k *Kernel) appendStepMessages(ctx context.Context, msgs []fantasy.Message) {
	if len(msgs) == 0 {
		return
	}
	k.StepHistoryStart = append(k.StepHistoryStart, len(k.History))
	k.History = append(k.History, msgs...)
	if b, err := json.Marshal(msgs); err == nil {
		_ = k.Fire(ctx, string(EventAssistantTurn), &AssistantTurnPayload{Messages: json.RawMessage(b)})
	}
}

// setWindowTokens updates the context-occupancy gauge from a single turn's usage
// (see windowTokens — one step's request size, not a summed multi-step total).
func (k *Kernel) setWindowTokens(u Usage) {
	k.usageMu.Lock()
	k.currentTokens = windowTokens(u)
	k.usageMu.Unlock()
}

// runToolCalls executes each tool call via the registry, firing pre/post events,
// and returns a tool-role message with the results plus a signature of
// (name, input, result) pairs for the loop guard.
func (k *Kernel) runToolCalls(ctx context.Context, tools []fantasy.AgentTool, calls []fantasy.ToolCallContent) (fantasy.Message, string) {
	toolMap := make(map[string]fantasy.AgentTool, len(tools))
	for _, t := range tools {
		toolMap[t.Info().Name] = t
	}

	var parts []fantasy.MessagePart
	var sig string
	for _, call := range calls {
		_ = k.Fire(ctx, string(EventPreToolUse), &ToolUsePayload{
			CallID: call.ToolCallID, Name: call.ToolName, Args: call.Input,
		})

		var output fantasy.ToolResultOutputContent
		var resultText string
		tool, ok := toolMap[call.ToolName]
		if !ok {
			resultText = "Error: tool not found: " + call.ToolName
			output = fantasy.ToolResultOutputContentText{Text: resultText}
		} else {
			resp, err := tool.Run(ctx, fantasy.ToolCall{ID: call.ToolCallID, Name: call.ToolName, Input: call.Input})
			if err != nil {
				resultText = "Error: " + err.Error()
			} else if resp.IsError {
				resultText = "Error: " + resp.Content
			} else {
				resultText = resp.Content
			}
			output = fantasy.ToolResultOutputContentText{Text: resultText}
		}

		parts = append(parts, fantasy.ToolResultPart{ToolCallID: call.ToolCallID, Output: output})

		payload := &ToolUseResultPayload{CallID: call.ToolCallID, Name: call.ToolName, Result: resultText}
		if len(resultText) >= 6 && resultText[:6] == "Error:" {
			payload.Error = resultText
			_ = k.Fire(ctx, string(EventPostToolUseFailure), payload)
		} else {
			_ = k.Fire(ctx, string(EventPostToolUse), payload)
		}

		sig += call.ToolName + "\x00" + call.Input + "\x00" + resultText + "\n"
	}
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts}, sig
}

// assistantMessageFromContent converts a Step's response content into an
// assistant message for history, mirroring Fantasy's own assembly so the wire
// shape is identical (text, reasoning, and tool-call parts are preserved).
func assistantMessageFromContent(rc fantasy.ResponseContent) fantasy.Message {
	var parts []fantasy.MessagePart
	for _, c := range rc {
		switch c.GetType() {
		case fantasy.ContentTypeText:
			if tc, ok := fantasy.AsContentType[fantasy.TextContent](c); ok {
				parts = append(parts, fantasy.TextPart{Text: tc.Text, ProviderOptions: fantasy.ProviderOptions(tc.ProviderMetadata)})
			}
		case fantasy.ContentTypeReasoning:
			if rcn, ok := fantasy.AsContentType[fantasy.ReasoningContent](c); ok {
				parts = append(parts, fantasy.ReasoningPart{Text: rcn.Text, ProviderOptions: fantasy.ProviderOptions(rcn.ProviderMetadata)})
			}
		case fantasy.ContentTypeToolCall:
			if tcc, ok := fantasy.AsContentType[fantasy.ToolCallContent](c); ok {
				parts = append(parts, fantasy.ToolCallPart{
					ToolCallID:       tcc.ToolCallID,
					ToolName:         tcc.ToolName,
					Input:            tcc.Input,
					ProviderExecuted: tcc.ProviderExecuted,
					ProviderOptions:  fantasy.ProviderOptions(tcc.ProviderMetadata),
				})
			}
		}
	}
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
}

// allEqual reports whether the last n entries of sigs are all identical and
// non-empty (used by the loop guard). A blank signature never trips the guard.
func allEqual(sigs []string, n int) bool {
	if len(sigs) < n || n <= 0 {
		return false
	}
	last := sigs[len(sigs)-1]
	if last == "" {
		return false
	}
	for i := 1; i < n; i++ {
		if sigs[len(sigs)-1-i] != last {
			return false
		}
	}
	return true
}
