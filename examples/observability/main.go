// Pattern: OBSERVABILITY EXPORT — build a trace and push it to Langfuse.
//
// This example records a synthetic-but-realistic trace directly into the store
// using the public API (no LLM calls needed), then exports it to Langfuse with
// toroid.LangfuseOTLP. It exercises the full projection: a user prompt (with an
// inline image ref), several tool calls including a failing one, token usage and
// cost, a nested subagent span, and compaction + notification events.
//
// Run it to verify the export end to end against a real Langfuse project:
//
//	export LANGFUSE_PUBLIC_KEY=pk-lf-…
//	export LANGFUSE_SECRET_KEY=sk-lf-…
//	export LANGFUSE_BASE_URL=https://<langfuse-host>
//	go run ./examples/observability
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/yashbonde/toroid-kernel/llm"
	toroid "github.com/yashbonde/toroid-kernel"
)

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Printf("set %s to run this example\n", name)
		os.Exit(1)
	}
	return v
}

func main() {
	base := mustEnv("LANGFUSE_BASE_URL")
	pub := mustEnv("LANGFUSE_PUBLIC_KEY")
	sec := mustEnv("LANGFUSE_SECRET_KEY")
	ctx := context.Background()

	st, err := toroid.NewStore()
	if err != nil {
		panic(err)
	}
	defer st.Close()

	now := time.Now().UnixNano()
	ms := int64(time.Millisecond)
	trace := toroid.NewSessionID()
	sub := toroid.NewSessionID() // subagent span
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	// img.jpg sits two dirs up (examples/img.jpg); reference it inline so the
	// trace input shows the ![](…) image reference.
	_, thisFile, _, _ := runtime.Caller(0)
	img := filepath.Join(filepath.Dir(thisFile), "..", "img.jpg")

	must(st.SaveTraceMeta(toroid.TraceMeta{TraceID: trace, Title: "refactor-and-verify", StartedAt: now}))
	must(st.SaveSpanMeta(toroid.SpanMeta{SpanID: trace, TraceID: trace, Title: "refactor-and-verify", Model: "llmgateway/kimi-k2p6", StartedAt: now, EndedAt: now + 8000*ms}))
	must(st.SaveSpanMeta(toroid.SpanMeta{SpanID: sub, TraceID: trace, ParentSpanID: trace, Title: "subagent: run-tests", Model: "llmgateway/kimi-k2p6", StartedAt: now + 3000*ms, EndedAt: now + 6000*ms}))
	must(st.AppendCost(trace, trace, 0.0124, 0.0124))
	must(st.AppendCost(trace, sub, 0.0033, 0.0033))

	ev := func(span string, seq uint64, ts int64, kind toroid.EventKind, payload any) {
		must(st.AppendEvent(trace, span, toroid.Event{Kind: kind, TraceID: trace, SpanID: span, EmitTS: ts, Seq: seq, Payload: payload}))
	}
	assistant := func(text, reasoning string, calls ...llm.ToolCallPart) toroid.AssistantTurnPayload {
		content := []llm.Part{}
		if reasoning != "" {
			content = append(content, llm.ReasoningPart{Text: reasoning})
		}
		content = append(content, llm.TextPart{Text: text})
		for _, c := range calls {
			content = append(content, c)
		}
		b, _ := json.Marshal([]llm.Message{{Role: llm.RoleAssistant, Parts: content}})
		return toroid.AssistantTurnPayload{Messages: b}
	}

	// Root span: turn 1 (read + failing vet), delegate to subagent, then final turn.
	ev(trace, 1, now+10*ms, toroid.EventUserPromptSubmit, toroid.UserPromptPayload{
		Prompt: "Refactor utils.go using this diagram ![](" + img + "), run the tests, and confirm they pass.",
	})
	ev(trace, 2, now+300*ms, toroid.EventPreToolUse, toroid.ToolUsePayload{CallID: "c1", Name: "read", Args: `{"filePath":"utils.go"}`})
	ev(trace, 3, now+360*ms, toroid.EventPostToolUse, toroid.ToolUseResultPayload{CallID: "c1", Name: "read", Result: "<path>utils.go</path> … 350 lines …"})
	ev(trace, 4, now+900*ms, toroid.EventPreToolUse, toroid.ToolUsePayload{CallID: "c2", Name: "bash", Args: `{"cmd":"go vet ./..."}`})
	ev(trace, 5, now+1500*ms, toroid.EventPostToolUseFailure, toroid.ToolUseResultPayload{CallID: "c2", Name: "bash", Error: "Error: exit 1: vet: utils.go:131:2: undefined: fooBar"})
	ev(trace, 6, now+1800*ms, toroid.EventTurnCost, toroid.TurnCostPayload{TurnUsage: toroid.Usage{Input: 4200, Output: 310, Reasoning: 120, CacheRead: 2000}, TurnCostUSD: 0.0061, TotalCostUSD: 0.0061})
	ev(trace, 7, now+1850*ms, toroid.EventAssistantTurn, assistant(
		"Found the duplication; fixing it, then delegating the test run.",
		"wrapInLogWidth and PrettyPrintHistory both re-pad — they can share one indent helper.",
		llm.ToolCallPart{ID: "c3", Name: "subagent_async", Arguments: `{"task":"run the test suite and report failures"}`},
	))
	ev(trace, 8, now+2900*ms, toroid.EventSubagentStart, toroid.SubagentPayload{SessionID: sub, Prompt: "run the test suite and report failures"})

	// Subagent span: its own prompt, a tool call, a turn.
	ev(sub, 1, now+3100*ms, toroid.EventUserPromptSubmit, toroid.UserPromptPayload{Prompt: "run the test suite and report failures"})
	ev(sub, 2, now+3400*ms, toroid.EventPreToolUse, toroid.ToolUsePayload{CallID: "s1", Name: "bash", Args: `{"cmd":"go test ./..."}`})
	ev(sub, 3, now+5200*ms, toroid.EventPostToolUse, toroid.ToolUseResultPayload{CallID: "s1", Name: "bash", Result: "ok  github.com/yashbonde/toroid-kernel  11 passed"})
	ev(sub, 4, now+5600*ms, toroid.EventTurnCost, toroid.TurnCostPayload{TurnUsage: toroid.Usage{Input: 1800, Output: 90}, TurnCostUSD: 0.0033, TotalCostUSD: 0.0033})
	ev(sub, 5, now+5700*ms, toroid.EventAssistantTurn, assistant("All 11 tests pass.", ""))

	// Root span: subagent completes, compaction happens, final turn.
	ev(trace, 9, now+6100*ms, toroid.EventTaskCompleted, toroid.TaskPayload{TaskID: "c3", Title: "run the test suite", Status: "completed"})
	ev(trace, 10, now+6200*ms, toroid.EventPreCompact, toroid.CompactPayload{MessageCount: 22, TokenCount: 41000})
	ev(trace, 11, now+6500*ms, toroid.EventPostCompact, toroid.CompactSummaryPayload{
		Summary: "Refactored utils.go to share an indent helper; subagent confirmed all tests pass.", MessagesBefore: 22, MessagesAfter: 3, TokensBefore: 41000,
	})
	ev(trace, 12, now+7000*ms, toroid.EventNotification, toroid.NotificationPayload{Title: "Task done", Message: "Refactor complete and tests green."})
	ev(trace, 13, now+7600*ms, toroid.EventTurnCost, toroid.TurnCostPayload{TurnUsage: toroid.Usage{Input: 3100, Output: 210}, TurnCostUSD: 0.0063, TotalCostUSD: 0.0124})
	ev(trace, 14, now+7700*ms, toroid.EventAssistantTurn, assistant(
		"Done: removed the duplicated indent logic in utils.go and the subagent confirmed all 11 tests pass.", ""))

	if err := toroid.LangfuseOTLP(ctx, trace, base, pub, sec); err != nil {
		panic(err)
	}
	tid, _ := toroid.OTELIDs(trace, trace)
	fmt.Printf("pushed trace to Langfuse: %s/project/<id>/traces/%s\n", base, tid.String())
}
