package toroid

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

// Kernel-owned tool loop. This drives each turn's LLM call through the Step
// layer — one product llm-step per turn — with the kernel executing tools
// between calls (§2 of the port scope): the Kernel owns turns/tools/compaction;
// a Step performs a single LLM call.
//
// Guarantees: MaxIter cap, repeat-call loop guard, queue-interrupt at turn
// boundaries, mid-turn compaction under context pressure, tool events, and
// per-turn cost.
//
// Turns use Step.Complete (non-streaming) so every llm-step can carry the
// gateway's authoritative cost header: the assistant text is written after each
// turn rather than token-by-token; reasoning content is emitted as one
// EventReasoning per turn.
func (k *Kernel) streamViaStep(ctx context.Context, w io.Writer, budget *spendBudget) error {
	model := ResolveModel(k.Cfg.Model)
	wireTools := k.wireTools()

	var recentSigs []string // rolling window of tool-call signatures for the loop guard
	turns := 0
	inspectionTurns := 0
	inspectionGuardSent := false
	workspaceMutated := false
	validationSinceMutation := false
	validationTurns := 0
	validationGuardSent := false
	completionGuardSent := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if budget.reached(k) {
			k.Logf("spend limit reached; stopping before the next llm-step")
			return nil
		}

		metadata := k.nextRequestMetadata(ctx)
		stepCtx := Context{
			SystemPrefix: k.SystemPromptPrefix,
			System:       k.SystemPrompt,
			Messages:     k.History,
			Tools:        wireTools,
			Metadata:     metadata,
		}
		k.fireTurnStarted(ctx, metadata)
		k.fireLLMStep(ctx, model.ID, stepCtx, "")
		res, err := k.Step.Complete(ctx, model, stepCtx, k.stepOpts())
		if err != nil {
			k.fireTurnFailed(ctx, metadata, err)
			return err
		}
		turns++

		// Record the assistant turn: append to history, bill, update occupancy.
		// Reasoning parts stay in history for observability; the wire layer drops
		// them when replaying (OpenAI assistant messages carry text + tool calls).
		normalizeToolCallPaths(res.Content, k.Cfg.WorkDir)
		k.appendStepMessages(ctx, []llm.Message{{Role: llm.RoleAssistant, Parts: res.Content}})
		k.recordUsage(ctx, res.Usage) // also refreshes the window-occupancy gauge
		if budget.reached(k) {
			k.Logf("spend limit reached; stopping agent loop")
			k.fireTurnCompleted(ctx, metadata, res.StopReason, len(res.ToolCalls()))
			return nil
		}

		for _, p := range res.Content {
			if rp, ok := p.(llm.ReasoningPart); ok && rp.Text != "" {
				_ = k.Fire(ctx, string(EventReasoning), &ReasoningPayload{Text: rp.Text})
			}
		}

		toolCalls := llm.ToolCallsOf(res.Content)

		// Write assistant text once the turn's tool intent is known: a final turn
		// (no tools) is the user-visible answer.
		if len(toolCalls) == 0 {
			if workspaceMutated && !validationSinceMutation && !completionGuardSent {
				k.fireTurnCompleted(ctx, metadata, res.StopReason, 0)
				k.History = append(k.History, llm.NewUserMessage(
					"Completion guard: files changed without validation after the last edit. "+
						"Run relevant validation, or verify that none applies, then finish.",
				))
				completionGuardSent = true
				continue
			}
			if _, err := io.WriteString(w, llm.TextOf(res.Content)); err != nil {
				k.fireTurnFailed(ctx, metadata, err)
				return err
			}
			k.fireTurnCompleted(ctx, metadata, res.StopReason, 0)
			return nil // end of chat
		}

		// Execute tools locally (not an llm-step) and append their results.
		toolMsg, sig := k.runToolCalls(ctx, model, toolCalls)
		k.appendStepMessages(ctx, []llm.Message{toolMsg})
		k.fireTurnCompleted(ctx, metadata, res.StopReason, len(toolCalls))

		// GLM can remain in repository-archeology mode long after it has enough
		// evidence to act. After a bounded inspection phase, inject one neutral
		// control-plane reminder. This is not a hard cap: genuinely missing
		// information can still be gathered, but the model must identify the new
		// question instead of repeating broad searches.
		mutated := toolCallsMutateFiles(toolCalls)
		validated := toolCallsRunValidation(toolCalls)
		if mutated {
			workspaceMutated = true
			validationSinceMutation = false
			validationTurns = 0
		} else if !workspaceMutated {
			inspectionTurns++
			if inspectionTurns >= 8 && !inspectionGuardSent {
				k.History = append(k.History, llm.NewUserMessage(
					"8 inspection turns, no edit. If evidence is sufficient, implement now; "+
						"otherwise do one targeted check for the missing fact.",
				))
				inspectionGuardSent = true
			}
		}
		if validated && workspaceMutated {
			validationSinceMutation = true
			validationTurns++
			if validationTurns >= 2 && !validationGuardSent {
				k.History = append(k.History, llm.NewUserMessage(
					"Validation guard: validation ran twice without an intervening edit. "+
						"Use those results; rerun only after a change or for a specific new reason.",
				))
				validationGuardSent = true
			}
		}

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
				k.History = append(k.History, llm.NewUserMessage(qm))
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

func toolCallsMutateFiles(calls []llm.ToolCallPart) bool {
	for _, call := range calls {
		switch call.Name {
		case "write", "edit", "multiedit":
			return true
		}
	}
	return false
}

func toolCallsRunValidation(calls []llm.ToolCallPart) bool {
	for _, call := range calls {
		if call.Name != "bash" {
			continue
		}
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Arguments), &args) != nil {
			continue
		}
		command := strings.ToLower(args.Command)
		for _, marker := range []string{
			"go test", "go build", "go vet", "pytest", "cargo test", "cargo check",
			"npm test", "npm run test", "pnpm test", "yarn test", "make test",
			"gradle test", "mvn test", "dotnet test",
		} {
			if strings.Contains(command, marker) {
				return true
			}
		}
	}
	return false
}

// normalizeToolCallPaths removes a redundant leading `cd <workdir> &&` from
// fresh bash calls. The bash tool already executes with cmd.Dir=WorkDir, so the
// prefix has no effect; retaining a long temporary clone path in every tool
// call only enlarges all subsequent prompts. Normalization happens before the
// call is executed or persisted, keeping live and reconstructed history equal.
func normalizeToolCallPaths(parts []llm.Part, workDir string) {
	if workDir == "" {
		return
	}
	prefixes := []string{
		"cd " + workDir + " && ",
		"cd \"" + workDir + "\" && ",
		"cd '" + workDir + "' && ",
	}
	for i, part := range parts {
		call, ok := part.(llm.ToolCallPart)
		if !ok || call.Name != "bash" {
			continue
		}
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Arguments), &args) != nil {
			continue
		}
		command := args.Command
		for _, prefix := range prefixes {
			if strings.HasPrefix(command, prefix) {
				args.Command = strings.TrimPrefix(command, prefix)
				b, err := json.Marshal(args)
				if err == nil {
					call.Arguments = string(b)
					parts[i] = call
				}
				break
			}
		}
	}
}

// wireTools returns the kernel's registered tools as wire descriptions for a
// Step Context, sorted by name: the registry is a map, and a shuffled tool
// order would change the request prefix every Run — busting the prompt cache.
func (k *Kernel) wireTools() []llm.Tool {
	if k.Tools == nil {
		return nil
	}
	var out []llm.Tool
	for _, t := range k.Tools.Tools() {
		if t.Handler != nil {
			out = append(out, llm.ToolOf(t.Handler))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// appendStepMessages appends a turn's messages to history and fires
// EventAssistantTurn so history can be reconstructed from events alone. Media
// bytes are stripped from the persisted payload — an image the model saw would
// otherwise be duplicated into SQLite (and the event bus) as base64 every time
// it appears in a turn; a resumed session gets a text stub instead.
func (k *Kernel) appendStepMessages(ctx context.Context, msgs []llm.Message) {
	if len(msgs) == 0 {
		return
	}
	k.History = append(k.History, msgs...)
	persist := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		persist[i] = m
		hasMedia := false
		for _, p := range m.Parts {
			switch v := p.(type) {
			case llm.FilePart:
				hasMedia = true
			case llm.ToolResultPart:
				hasMedia = hasMedia || len(v.Files) > 0
			}
		}
		if !hasMedia {
			continue
		}
		parts := make([]llm.Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch v := p.(type) {
			case llm.FilePart:
				parts = append(parts, llm.TextPart{Text: "[media omitted: " + v.Filename + "]"})
			case llm.ToolResultPart:
				if len(v.Files) > 0 {
					v.Files = nil
					v.Content += " [media omitted]"
				}
				parts = append(parts, v)
			default:
				parts = append(parts, p)
			}
		}
		persist[i].Parts = parts
	}
	if b, err := json.Marshal(persist); err == nil {
		_ = k.Fire(ctx, string(EventAssistantTurn), &AssistantTurnPayload{Messages: json.RawMessage(b)})
	}
}

// runToolCalls executes each tool call via the registry, firing pre/post events,
// and returns a tool-role message with the results plus a signature of
// (name, input, result) pairs for the loop guard.
func (k *Kernel) runToolCalls(ctx context.Context, model Model, calls []llm.ToolCallPart) (llm.Message, string) {
	var parts []llm.Part
	var sig string
	for _, call := range calls {
		_ = k.Fire(ctx, string(EventPreToolUse), &ToolUsePayload{
			CallID: call.ID, Name: call.Name, Args: call.Arguments,
		})

		var resultText string
		var files []llm.FilePart
		tool, ok := k.Tools.Lookup(call.Name)
		if !ok || tool.Handler == nil {
			resultText = "Error: tool not found: " + call.Name
		} else {
			res, err := tool.Handler.Run(ctx, call.Arguments)
			if err != nil {
				resultText = "Error: " + err.Error()
			} else if res.IsError && !strings.HasPrefix(res.Content, "Error:") {
				resultText = "Error: " + res.Content
			} else {
				resultText = res.Content
				files = res.Files
			}
		}
		if len(files) > 0 && !model.SupportsImage() {
			// Never ship media to a text-only model: it would either error or be
			// silently dropped upstream while still being paid for on the wire.
			files = nil
			resultText += " [media omitted: model does not accept image input]"
		}

		parts = append(parts, llm.ToolResultPart{
			ToolCallID: call.ID,
			Content:    resultText,
			IsError:    strings.HasPrefix(resultText, "Error:"),
			Files:      files,
		})

		payload := &ToolUseResultPayload{CallID: call.ID, Name: call.Name, Result: resultText}
		if strings.HasPrefix(resultText, "Error:") {
			payload.Error = resultText
			_ = k.Fire(ctx, string(EventPostToolUseFailure), payload)
		} else {
			_ = k.Fire(ctx, string(EventPostToolUse), payload)
		}

		h := fnv.New64a()
		h.Write([]byte(call.Name))
		h.Write([]byte{0})
		h.Write([]byte(call.Arguments))
		h.Write([]byte{0})
		h.Write([]byte(resultText))
		sig += strconv.FormatUint(h.Sum64(), 16) + "\n"
	}
	return llm.Message{Role: llm.RoleTool, Parts: parts}, sig
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
