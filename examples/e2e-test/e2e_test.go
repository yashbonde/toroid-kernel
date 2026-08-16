package e2etest

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// recordingStep keeps the stable portion of every loop request so this test
// catches accidental prompt-cache busting when optional capabilities are on.
type recordingStep struct {
	*toroid.FauxStep
	contexts []toroid.Context
}

func (s *recordingStep) record(c toroid.Context) {
	s.contexts = append(s.contexts, toroid.Context{
		System: c.System,
		Tools:  append([]llm.Tool(nil), c.Tools...),
	})
}

func (s *recordingStep) Complete(ctx context.Context, model toroid.Model, c toroid.Context, opts toroid.StepOptions) (*toroid.AssistantMessage, error) {
	s.record(c)
	return s.FauxStep.Complete(ctx, model, c, opts)
}

func (s *recordingStep) Stream(ctx context.Context, model toroid.Model, c toroid.Context, opts toroid.StepOptions) (*toroid.StreamResult, error) {
	s.record(c)
	return s.FauxStep.Stream(ctx, model, c, opts)
}

// TestKernelE2E drives a real Kernel through capability discovery, three tool
// adapters, the kernel-owned loop, event persistence, billing, and structured
// output. The model is scripted and the MCP server is local, so it needs no
// credentials or internet access.
func TestKernelE2E(t *testing.T) {
	ctx := context.Background()

	// Isolate discovery from the developer's real ~/.toroid, then install the
	// committed example skill exactly as a host application would.
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillFixture, err := os.ReadFile(filepath.Join("skills", "e2e-review.md"))
	if err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(home, ".toroid", "skills")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "e2e-review.md")
	if err := os.WriteFile(skillPath, skillFixture, 0644); err != nil {
		t.Fatal(err)
	}

	// Exercise the actual streamable-HTTP MCP handshake, discovery, namespace,
	// and call path against a process-local fixture server.
	mcpToolRan := 0
	mcpServer := mcpserver.NewMCPServer("e2e-mcp", "1.0.0")
	mcpServer.AddTool(mcp.Tool{
		Name:        "echo",
		Description: "echo through MCP",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"message": map[string]any{"type": "string"},
			},
			Required: []string{"message"},
		},
	}, func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mcpToolRan++
		return mcp.NewToolResultText("mcp echoed: " + request.GetString("message", "")), nil
	})
	mcpHTTP := httptest.NewServer(mcpserver.NewStreamableHTTPServer(mcpServer))
	defer mcpHTTP.Close()

	hostToolRan := 0
	reg := tools.NewRegistry()
	reg.Register(&tools.ToolDef{
		Name: "echo", Description: "echo the message",
		Handler: llm.NewTool("echo", "echo the message",
			func(ctx context.Context, in struct {
				Msg string `json:"msg"`
			}) (llm.ToolResult, error) {
				hostToolRan++
				return llm.NewTextResult("echoed: " + in.Msg), nil
			}),
	})

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                "llmgateway/claude-haiku-4-5",
		WorkDir:              t.TempDir(),
		IncludeComputerTools: true,
		IncludeSubagentTools: true,
		Tools:                reg,
		MCPServers: []tools.MCPServerConfig{{
			Name:    "fixture",
			BaseURL: mcpHTTP.URL + "/mcp",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()

	step := &recordingStep{FauxStep: &toroid.FauxStep{
		Replies: []toroid.FauxReply{
			{ToolCalls: []toroid.FauxToolCall{{ID: "c1", Name: "skill", Input: `{"path":` + string(mustJSON(t, skillPath)) + `}`}}, Usage: llm.Usage{Input: 900, Output: 20}, Cost: 0.002},
			{ToolCalls: []toroid.FauxToolCall{{ID: "c2", Name: "fixture__echo", Input: `{"message":"hello"}`}}, Usage: llm.Usage{Input: 950, Output: 20}, Cost: 0.002},
			{ToolCalls: []toroid.FauxToolCall{{ID: "c3", Name: "echo", Input: `{"msg":"hi"}`}}, Usage: llm.Usage{Input: 1000, Output: 20}, Cost: 0.002},
			{Text: "all done", Usage: llm.Usage{Input: 1100, Output: 30}, Cost: 0.003},
		},
		Object: toroid.FauxObject{Object: map[string]any{"ok": true}, Usage: llm.Usage{Input: 500, Output: 10}, Cost: 0.001},
	}}
	k.Step = step

	// v2: a turn's structured content is TurnCompleted.Content — what a
	// separate AssistantTurn event used to carry.
	var turnPayloads []json.RawMessage
	k.On(toroid.EventTurnCompleted, func(_ context.Context, e toroid.Event) error {
		if content := e.Payload.(*toroid.TurnPayload).Content; len(content) > 0 {
			turnPayloads = append(turnPayloads, content)
		}
		return nil
	})

	out, usage, err := k.Run(ctx, "load the skill, use MCP, then echo")
	if err != nil {
		t.Fatal(err)
	}
	if out != "all done" {
		t.Errorf("final answer = %q, want all done", out)
	}
	if hostToolRan != 1 || mcpToolRan != 1 {
		t.Errorf("tool calls: host=%d MCP=%d, want one each", hostToolRan, mcpToolRan)
	}
	if len(k.History) != 8 {
		t.Errorf("history len = %d, want 8", len(k.History))
	}
	if !historyHasToolResult(k.History, "E2E_SKILL_BODY") {
		t.Error("skill body was not loaded through the skill tool")
	}
	if !historyHasToolResult(k.History, "mcp echoed: hello") {
		t.Error("MCP tool result missing from history")
	}
	if k.RunningCostUSD() <= 0 || len(usage.Tokens) == 0 {
		t.Error("tool-loop llm-steps were not billed")
	}

	if len(turnPayloads) < 4 {
		t.Fatalf("expected at least 4 TurnCompleted events with content, got %d", len(turnPayloads))
	}
	var sawToolCall bool
	for _, raw := range turnPayloads {
		var messages []llm.Message
		if err := json.Unmarshal(raw, &messages); err != nil {
			t.Fatalf("TurnCompleted content does not parse as []llm.Message: %v", err)
		}
		for _, message := range messages {
			sawToolCall = sawToolCall || len(message.ToolCalls()) > 0
		}
	}
	if !sawToolCall {
		t.Error("no persisted turn carried a tool call")
	}

	costBefore := k.RunningCostUSD()
	var buf strings.Builder
	err = k.Stream(ctx, "return it as JSON", &buf, toroid.WithSchema(
		toroid.Schema{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
		"result", "the result"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"ok":true`) {
		t.Errorf("schema output = %q", buf.String())
	}
	if k.RunningCostUSD() <= costBefore {
		t.Error("object llm-step was not billed")
	}

	// History grows only after the stable system/tool cache prefix. Optional
	// capabilities must not alter that prefix between turns or later Runs.
	if len(step.contexts) < 5 {
		t.Fatalf("recorded %d loop contexts, want at least 5", len(step.contexts))
	}
	wantSystem := step.contexts[0].System
	wantTools, err := json.Marshal(step.contexts[0].Tools)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(wantSystem, "# Available capabilities") != 1 {
		t.Fatalf("capability reminder count = %d, want 1", strings.Count(wantSystem, "# Available capabilities"))
	}
	for i, got := range step.contexts {
		if got.System != wantSystem {
			t.Errorf("context %d changed the stable system prefix", i)
		}
		gotTools, err := json.Marshal(got.Tools)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotTools) != string(wantTools) {
			t.Errorf("context %d changed the stable tool prefix", i)
		}
	}
	for _, message := range k.History {
		if strings.Contains(message.Text(), "# Available capabilities") {
			t.Error("stable capability reminder was re-injected into message history")
		}
	}
}

func TestSpendLimitsE2E(t *testing.T) {
	newKernel := func(t *testing.T, transcriptMax float64, replies []toroid.FauxReply) (*toroid.Kernel, *recordingStep, *int) {
		t.Helper()
		toolRuns := 0
		reg := tools.NewRegistry()
		reg.Register(&tools.ToolDef{
			Name: "echo", Description: "echo the message",
			Handler: llm.NewTool("echo", "echo the message",
				func(context.Context, struct {
					Msg string `json:"msg"`
				}) (llm.ToolResult, error) {
					toolRuns++
					return llm.NewTextResult("ok"), nil
				}),
		})
		loadSkills := false
		k, err := toroid.NewKernel(context.Background(), toroid.Config{
			Model:                 "llmgateway/claude-haiku-4-5",
			WorkDir:               t.TempDir(),
			IncludeComputerTools:  false,
			LoadSkills:            &loadSkills,
			Tools:                 reg,
			MaxTranscriptSpendUSD: transcriptMax,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k.Close() })
		step := &recordingStep{FauxStep: &toroid.FauxStep{
			Replies: replies,
			Object:  toroid.FauxObject{Object: map[string]any{"ok": true}, Cost: 0.01},
		}}
		k.Step = step
		return k, step, &toolRuns
	}

	toolReply := func(id string, cost float64) toroid.FauxReply {
		return toroid.FauxReply{
			ToolCalls: []toroid.FauxToolCall{{ID: id, Name: "echo", Input: `{"msg":"x"}`}},
			Cost:      cost,
		}
	}

	t.Run("turn limit stops tools and structured output", func(t *testing.T) {
		k, step, toolRuns := newKernel(t, 0, []toroid.FauxReply{
			toolReply("c1", 0.01),
			toolReply("c2", 0.01),
			toolReply("c3", 0.01),
			{Text: "too late", Cost: 0.01},
		})
		var out strings.Builder
		err := k.Stream(context.Background(), "work", &out,
			toroid.WithMaxTurnSpendUSD(0.025),
			toroid.WithSchema(toroid.Schema{"type": "object"}, "result", "result"))
		if err != nil {
			t.Fatal(err)
		}
		if len(step.contexts) != 3 || *toolRuns != 2 {
			t.Fatalf("llm steps=%d tool runs=%d, want 3 and 2", len(step.contexts), *toolRuns)
		}
		if out.Len() != 0 {
			t.Fatalf("structured-output call ran after limit: %q", out.String())
		}
	})

	t.Run("transcript limit blocks the next call", func(t *testing.T) {
		k, step, toolRuns := newKernel(t, 0.025, []toroid.FauxReply{
			toolReply("c1", 0.01),
			{Text: "first done", Cost: 0.01},
			toolReply("c2", 0.01),
			{Text: "too late", Cost: 0.01},
		})
		if _, _, err := k.Run(context.Background(), "first"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := k.Run(context.Background(), "second"); err != nil {
			t.Fatal(err)
		}
		stepsAtLimit := len(step.contexts)
		if _, _, err := k.Run(context.Background(), "third"); err != nil {
			t.Fatal(err)
		}
		if len(step.contexts) != stepsAtLimit || len(step.contexts) != 3 || *toolRuns != 1 {
			t.Fatalf("llm steps=%d tool runs=%d, want 3 and 1 with no third-call llm-step", len(step.contexts), *toolRuns)
		}
	})
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func historyHasToolResult(history []llm.Message, substring string) bool {
	for _, message := range history {
		for _, part := range message.Parts {
			if result, ok := part.(llm.ToolResultPart); ok && strings.Contains(result.Content, substring) {
				return true
			}
		}
	}
	return false
}
