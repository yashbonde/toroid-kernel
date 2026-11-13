package toroid

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

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
// By default turns use Step.Complete (non-streaming) so every llm-step carries
// the gateway's cost header: the assistant text is written after each turn
// rather than token-by-token. A run that opts in with WithTokenStream uses
// Step.Stream instead and writes text as it arrives (see stepTurn). Either way
// reasoning content is emitted (and persisted) as one EventReasoning block per
// turn.
func (k *Kernel) streamViaStep(ctx context.Context, w io.Writer, tokens *tokenStream, budget *spendBudget) error {
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
		k.fireTurnStarted(ctx, metadata, model.ID, stepCtx, "")
		res, textSent, err := k.stepTurn(ctx, model, stepCtx, w, tokens)
		if err != nil {
			k.fireTurnFailed(ctx, metadata, err)
			return err
		}
		turns++

		// Record the assistant turn: append to history, bill, update occupancy.
		// Reasoning parts stay in history for observability; the wire layer drops
		// them when replaying (OpenAI assistant messages carry text + tool calls).
		normalizeToolCallPaths(res.Content, k.Cfg.WorkDir)
		persist := k.appendStepMessages(ctx, []llm.Message{{Role: llm.RoleAssistant, Parts: res.Content}})
		totalCostUSD := k.recordUsage(res.Usage) // also refreshes the window-occupancy gauge
		if budget.reached(k) {
			k.Logf("spend limit reached; stopping agent loop")
			k.fireTurnCompleted(ctx, metadata, res.StopReason, len(res.ToolCalls()), marshalMessages(persist), res.Usage, totalCostUSD)
			return nil
		}

		for _, p := range res.Content {
			if rp, ok := p.(llm.ReasoningPart); ok && rp.Text != "" {
				_ = k.FireInTurn(ctx, string(EventReasoning), metadata, &ReasoningPayload{Text: rp.Text})
			}
		}

		toolCalls := llm.ToolCallsOf(res.Content)

		// Write assistant text once the turn's tool intent is known: a final turn
		// (no tools) is the user-visible answer.
		if len(toolCalls) == 0 {
			if workspaceMutated && !validationSinceMutation && !completionGuardSent {
				k.fireTurnCompleted(ctx, metadata, res.StopReason, 0, marshalMessages(persist), res.Usage, totalCostUSD)
				k.fireGuard(ctx, metadata, "completion", "files changed without an intervening validation run")
				k.History = append(k.History, llm.NewUserMessage(
					"Completion guard: files changed without validation after the last edit. "+
						"Run relevant validation, or verify that none applies, then finish.",
				))
				completionGuardSent = true
				continue
			}
			if !textSent {
				if _, err := io.WriteString(w, llm.TextOf(res.Content)); err != nil {
					k.fireTurnFailed(ctx, metadata, err)
					return err
				}
			}
			k.fireTurnCompleted(ctx, metadata, res.StopReason, 0, marshalMessages(persist), res.Usage, totalCostUSD)
			return nil // end of chat
		}

		// Write assistant text before executing tools so the front-end (TUI, --run)
		// shows the model's reasoning or explanation alongside tool calls.
		if text := llm.TextOf(res.Content); text != "" && !textSent {
			if _, err := io.WriteString(w, text); err != nil {
				k.fireTurnFailed(ctx, metadata, err)
				return err
			}
		}

		// Execute tools locally (not an llm-step) and append their results.
		toolMsg, sig, visionUsage := k.runToolCalls(ctx, model, metadata, toolCalls, latestIntent(res.Content, k.History[:len(k.History)-1]))
		persist = append(persist, k.appendStepMessages(ctx, []llm.Message{toolMsg})...)
		turnUsage := res.Usage
		if visionUsage != nil {
			// A vision delegate's calls belong to this turn: bill them to the
			// session and report them in the turn's usage and cost.
			totalCostUSD = k.recordSideUsage(*visionUsage)
			turnUsage = addUsage(turnUsage, *visionUsage)
		}
		k.fireTurnCompleted(ctx, metadata, res.StopReason, len(toolCalls), marshalMessages(persist), turnUsage, totalCostUSD)

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
				k.fireGuard(ctx, metadata, "inspection", "8 inspection turns with no edit")
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
				k.fireGuard(ctx, metadata, "validation", "validation ran twice without an intervening edit")
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
				reason := fmt.Sprintf("%d consecutive identical tool calls with identical results", n)
				k.fireGuard(ctx, metadata, "loop_guard", reason)
				k.Logf("loop guard: stopping after " + reason)
				return nil
			}
		}

		// MaxIter cap on tool-call turns.
		if k.Cfg.MaxIter > 0 && turns >= k.Cfg.MaxIter {
			reason := fmt.Sprintf("reached MaxIter=%d", k.Cfg.MaxIter)
			k.fireGuard(ctx, metadata, "max_iter", reason)
			k.Logf("step loop: " + reason)
			return nil
		}

		// Queue interrupt at this safe turn boundary: inject queued messages and
		// continue the same chat on the extended history.
		if queued := k.drainQueue(); len(queued) > 0 {
			for _, qm := range queued {
				k.History = append(k.History, llm.NewUserMessage(qm))
			}
			_ = k.FireInTurn(ctx, string(EventQueueInterrupt), metadata, &QueueInterruptPayload{Messages: queued})
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

// tokenStream is a run's opt-in to live tokens (WithTokenStream,
// WithReasoningStream). A nil *tokenStream means the default: one Step.Complete
// per turn, with the turn's text written once it ends.
type tokenStream struct {
	reasoning io.Writer // optional; receives reasoning deltas as they arrive
}

// stepTurn runs one turn's llm-step. Without a token stream it is a plain
// Step.Complete. With one it uses Step.Stream, writing text deltas to w (and
// reasoning deltas to tokens.reasoning) as they arrive. textSent reports whether
// any of the turn's text already reached w, so the caller does not write it
// again.
func (k *Kernel) stepTurn(ctx context.Context, model Model, c Context, w io.Writer, tokens *tokenStream) (res *AssistantMessage, textSent bool, err error) {
	if tokens == nil {
		res, err = k.Step.Complete(ctx, model, c, k.stepOpts())
		return res, false, err
	}
	stream, err := k.Step.Stream(ctx, model, c, k.stepOpts())
	if err != nil {
		return nil, false, err
	}
	var writeErr error
	for d := range stream.Deltas {
		// Keep draining after a failed write: the producer blocks until each
		// delta is read or ctx ends, so abandoning the channel would leak it.
		if writeErr != nil {
			continue
		}
		if d.Text != "" {
			textSent = true
			_, writeErr = io.WriteString(w, d.Text)
		}
		if d.Reasoning != "" && tokens.reasoning != nil && writeErr == nil {
			_, writeErr = io.WriteString(tokens.reasoning, d.Reasoning)
		}
	}
	res, err = stream.Final()
	if err == nil {
		err = writeErr
	}
	return res, textSent, err
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

// modelAcceptsImages reports whether tool-result media may be sent to the
// current model: Config.ImageInputOverride when the host set it, else the
// model-capability table.
func (k *Kernel) modelAcceptsImages(model Model) bool {
	if k.Cfg.ImageInputOverride != nil {
		return *k.Cfg.ImageInputOverride
	}
	return model.SupportsImage()
}

// appendStepMessages appends a turn's messages to history and fires
// EventAssistantTurn so history can be reconstructed from events alone. Media
// bytes are stripped from the persisted payload — an image the model saw would
// otherwise be duplicated into SQLite (and the event bus) as base64 every time
// it appears in a turn; a resumed session gets a text stub instead.
func (k *Kernel) appendStepMessages(ctx context.Context, msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return nil
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
	return persist
}

// marshalMessages serializes a turn's persist-ready messages for
// TurnPayload.Content — what EventAssistantTurn used to carry as its own
// event. Returns nil (omitted from the payload) on marshal failure or an
// empty slice, rather than emitting a separate event just to report "none".
func marshalMessages(msgs []llm.Message) json.RawMessage {
	if len(msgs) == 0 {
		return nil
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// runToolCalls executes each tool call via the registry, firing pre/post events,
// and returns a tool-role message with the results plus a signature of
// (name, input, result) pairs for the loop guard.
//
// When a model replies with several tool calls in one assistant message they
// are executed concurrently, but not blindly: calls that mutate shared
// filesystem state (write/edit/multiedit) are isolated from the parallel wave
// so a dependent read or a test run never sees a half-written file.
//
// Execution order within a batch:
//   - Announced: every PreToolUse fires up front, in input order.
//   - Mutating calls run strictly sequentially (one at a time, in input order).
//   - Read-only calls run in parallel goroutines after the mutators, and
//     overlap with each other.
//   - Results are reassembled in the caller's original order and each call's
//     completion event fires, so the tool-role message and the loop-guard
//     signature stay deterministic no matter the interleaving.
//
// With a vision delegate (Config.Vision), a call whose result carries media has
// that media described by the vision model inside its own goroutine, so
// several screenshots in one batch are described in parallel; intent is the
// main model's latest assistant text, passed along so the vision model knows
// what to look for. visionUsage is the sum of those calls' usage, or nil when
// none ran; each one also fires EventVisionDescribed, in call order.
func (k *Kernel) runToolCalls(ctx context.Context, model Model, metadata llm.RequestMetadata, calls []llm.ToolCallPart, intent string) (llm.Message, string, *Usage) {
	// Announce every call before any of them start, in input order, so
	// consumers see the full batch begin as a unit.
	for _, call := range calls {
		_ = k.Fire(ctx, string(EventPreToolUse), &ToolUsePayload{
			CallID: call.ID, Name: call.Name, Args: call.Arguments,
		})
	}

	type result struct {
		resultText string
		sigText    string // the tool's own text, before any vision description
		files      []llm.FilePart
		vision     *visionOutcome
	}
	results := make([]result, len(calls))
	request := ""
	if k.visionEnabled() {
		request = latestUserRequest(k.History)
	}

	execute := func(i int, call llm.ToolCallPart) {
		var resultText, question string
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
				question = res.Question
			}
		}
		sigText := resultText
		if len(files) > 0 && k.visionEnabled() {
			// The main model never sees the media: the vision delegate describes
			// it, and its answer (or why it failed) joins the tool's text.
			_ = k.FireInTurn(ctx, string(EventVisionStarted), metadata, &VisionPayload{
				CallID: call.ID, Name: call.Name, Model: k.Cfg.Vision.Model,
			})
			desc, outcome := k.describeMedia(ctx, metadata, visionRequest{
				tool: call.Name, args: call.Arguments, toolText: resultText,
				question: question, intent: intent, request: request, files: files,
			})
			resultText = withVisionText(resultText, outcome.model, desc, outcome.err)
			results[i] = result{resultText: resultText, sigText: sigText, vision: &outcome}
			return
		}
		if len(files) > 0 && !k.modelAcceptsImages(model) {
			// Never ship media to a text-only model: it would either error or be
			// silently dropped upstream while still being paid for on the wire.
			files = nil
			resultText += " [media omitted: model does not accept image input]"
		}
		results[i] = result{resultText: resultText, sigText: sigText, files: files}
	}

	// Separate mutating calls so they run first and strictly sequentially —
	// a paired read or go test in the same batch must observe the finished edit.
	var mutating, readonly []int
	for i, call := range calls {
		if mutatingTool(call.Name) {
			mutating = append(mutating, i)
		} else {
			readonly = append(readonly, i)
		}
	}
	for _, i := range mutating {
		execute(i, calls[i])
	}

	var wg sync.WaitGroup
	for _, i := range readonly {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			execute(i, calls[i])
		}(i)
	}
	wg.Wait()

	// Reassemble results in the original call order and fire each call's
	// completion event.
	var parts []llm.Part
	var sig string
	var visionUsage *Usage
	for i, call := range calls {
		resultText := results[i].resultText
		files := results[i].files

		if v := results[i].vision; v != nil {
			if visionUsage == nil {
				visionUsage = &Usage{PricingOK: true}
			}
			// A call that failed before any response bills nothing; skipping it
			// keeps the turn's known cost from turning into "unknown".
			if v.err == nil || v.usage != (Usage{}) {
				*visionUsage = addUsage(*visionUsage, v.usage)
			}
			payload := &VisionPayload{CallID: call.ID, Name: call.Name, Model: v.model, Usage: v.usage, CostUSD: v.usage.Cost}
			if v.err != nil {
				payload.Error = v.err.Error()
			}
			_ = k.FireInTurn(ctx, string(EventVisionDescribed), metadata, payload)
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
		// The loop guard hashes the tool's own text: a vision model's wording
		// varies from call to call and would hide a genuine spin.
		h.Write([]byte(results[i].sigText))
		sig += strconv.FormatUint(h.Sum64(), 16) + "\n"
	}
	return llm.Message{Role: llm.RoleTool, Parts: parts}, sig, visionUsage
}

// mutatingTool reports whether a tool name mutates shared filesystem state.
// These are run sequentially (never concurrently) so a batch's dependent reads
// and validation see deterministic file state.
func mutatingTool(name string) bool {
	switch name {
	case "write", "edit", "multiedit":
		return true
	}
	return false
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
