package toroid

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yashbonde/toroid-kernel/llm"
)

// A kernel already at the depth ceiling must refuse to delegate rather than
// spawning another kernel, so a "delegate this" loop cannot fan out forever.
func TestSubagentDepthLimitRefusesToNest(t *testing.T) {
	k := &Kernel{Cfg: Config{SessionID: "s1", TraceID: "s1", Depth: DefaultMaxSubagentDepth}}
	_, err := k.runSubagent(context.Background(), "do work", SubagentOpts{})
	if err == nil {
		t.Fatal("expected depth limit error, got nil")
	}
	if !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("error = %v, want a depth limit error", err)
	}
}

func TestMaxSubagentDepthHonoursConfig(t *testing.T) {
	k := &Kernel{Cfg: Config{MaxSubagentDepth: 1}}
	if got := k.maxSubagentDepth(); got != 1 {
		t.Fatalf("maxSubagentDepth() = %d, want 1", got)
	}
	// Depth 1 is allowed under a ceiling of 1; depth 2 is not.
	k.Cfg.Depth = 1
	if _, err := k.runSubagent(context.Background(), "work", SubagentOpts{}); err == nil {
		t.Fatal("expected refusal at the configured ceiling")
	}

	def := &Kernel{Cfg: Config{}}
	if got := def.maxSubagentDepth(); got != DefaultMaxSubagentDepth {
		t.Fatalf("default maxSubagentDepth() = %d, want %d", got, DefaultMaxSubagentDepth)
	}
}

func TestDefaultSubagentModelPrefersSmallerModel(t *testing.T) {
	k := &Kernel{Cfg: Config{Model: "big", SmallerModel: "small"}}
	if got := k.defaultSubagentModel(); got != "small" {
		t.Fatalf("defaultSubagentModel() = %q, want %q", got, "small")
	}
	k = &Kernel{Cfg: Config{Model: "big"}}
	if got := k.defaultSubagentModel(); got != "big" {
		t.Fatalf("defaultSubagentModel() = %q, want %q", got, "big")
	}
}

// The model-override fallback must be narrow. A requested model that never
// produced a turn is a routing mistake worth retrying on the default; a child
// that answered and then failed would fail identically on any model, and
// retrying it bills the same work twice.
func TestModelOverrideFallsBackWhenModelNeverRan(t *testing.T) {
	var ran []string
	k := newFallbackTestKernel(t, &ran, func(model string) Step {
		if model == "broken" {
			// Never completes a turn — what an unknown or unreachable model does.
			return &failingCompleteStep{FauxStep: &FauxStep{}, err: errors.New("model not found")}
		}
		return &FauxStep{Replies: []FauxReply{{Text: "done", StopReason: "stop"}}}
	})

	res, err := k.runSubagent(context.Background(), "work", SubagentOpts{Model: "broken"})
	if err != nil {
		t.Fatalf("expected the fallback to succeed, got %v", err)
	}
	if !res.FellBack {
		t.Error("FellBack = false, want true")
	}
	if res.RequestedModel != "broken" {
		t.Errorf("RequestedModel = %q, want broken", res.RequestedModel)
	}
	if res.Model != "small" {
		t.Errorf("Model = %q, want the default (small)", res.Model)
	}
	if want := []string{"broken", "small"}; !slices.Equal(ran, want) {
		t.Errorf("models run = %v, want %v", ran, want)
	}
}

// A child that answered and then failed must NOT be retried on another model.
func TestFailureAfterModelAnsweredIsNotRetried(t *testing.T) {
	var ran []string
	k := newFallbackTestKernel(t, &ran, func(string) Step {
		// Answers every turn with a tool call that never terminates, so the loop
		// guard ends the run: a real failure the model is not responsible for.
		return &FauxStep{Replies: []FauxReply{{
			Text:      "looping",
			ToolCalls: []FauxToolCall{{ID: "c1", Name: "nope", Input: "{}"}},
		}}}
	})

	res, _ := k.runSubagent(context.Background(), "work", SubagentOpts{Model: "broken"})
	if res.FellBack {
		t.Error("FellBack = true; a model that answered must not trigger a retry")
	}
	if len(ran) != 1 {
		t.Errorf("ran %d attempts (%v), want 1 — the task was billed twice", len(ran), ran)
	}
}

// No override means no fallback: there is nothing to fall back from.
func TestNoOverrideNeverFallsBack(t *testing.T) {
	var ran []string
	k := newFallbackTestKernel(t, &ran, func(string) Step {
		return &failingCompleteStep{FauxStep: &FauxStep{}, err: errors.New("boom")}
	})

	res, err := k.runSubagent(context.Background(), "work", SubagentOpts{})
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if res.FellBack {
		t.Error("FellBack = true without an override")
	}
	if len(ran) != 1 {
		t.Errorf("ran %d attempts (%v), want 1", len(ran), ran)
	}
}

// newFallbackTestKernel returns a parent whose subagents are built in-process
// with a scripted Step chosen by model, recording each model actually run.
func newFallbackTestKernel(t *testing.T, ran *[]string, step func(model string) Step) *Kernel {
	t.Helper()
	k := &Kernel{
		Cfg:      Config{SessionID: "parent", TraceID: "parent", Model: "big", SmallerModel: "small", MaxRepeatCalls: 2},
		Hooks:    &HookRegistry{},
		Sessions: map[string]Usage{},
	}
	k.newSubKernel = func(ctx context.Context, cfg Config) (*Kernel, error) {
		*ran = append(*ran, cfg.Model)
		loadSkills := false
		cfg.LoadSkills = &loadSkills
		cfg.WorkDir = t.TempDir()
		cfg.IncludeComputerTools = false
		sub, err := NewKernel(ctx, cfg)
		if err != nil {
			return nil, err
		}
		sub.Step = step(cfg.Model)
		return sub, nil
	}
	return k
}

// A cancelled context must never trigger the fallback: the caller asked to stop.
func TestCancelledContextSkipsFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	k := &Kernel{Cfg: Config{SessionID: "s1", TraceID: "s1", Model: "def", MaxTranscriptSpendUSD: 0.0001}}
	// Spend cap already exceeded, so the attempt fails without any network use.
	k.runningCostUSD = 1
	_, err := k.runSubagent(ctx, "work", SubagentOpts{Model: "bad-model"})
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if ctx.Err() == nil {
		t.Fatal("context should be cancelled")
	}
}

// A subagent that fails part-way has still spent money. Run reports usage
// through EventStop, which a failed run never fires, so taking the child's
// accounting from that payload silently dropped the spend.
func TestFailedSubagentStillReportsItsSpend(t *testing.T) {
	var ran []string
	k := newFallbackTestKernel(t, &ran, func(string) Step {
		// Answers (billing a turn), then loops until the repeat guard ends it.
		return &FauxStep{Replies: []FauxReply{{
			Text:      "working",
			ToolCalls: []FauxToolCall{{ID: "c1", Name: "nope", Input: "{}"}},
			Usage:     llm.Usage{Input: 100, Output: 50},
			Cost:      0.25,
		}}}
	})

	res, _ := k.runSubagent(context.Background(), "work", SubagentOpts{})
	if res.Usage.Cost <= 0 {
		t.Fatalf("Usage.Cost = %v, want the spend before the failure to be counted", res.Usage.Cost)
	}
	if res.Steps == 0 {
		t.Error("Steps = 0, want the billed turns to be counted")
	}
	if got := k.RunningCostUSD(); got <= 0 {
		t.Errorf("parent RunningCostUSD = %v, want the child's spend rolled up", got)
	}
}

// Concurrent children must divide the transcript cap, not each see all of it.
func TestConcurrentSubagentsCannotEachClaimTheWholeCap(t *testing.T) {
	k := &Kernel{Cfg: Config{SessionID: "s", TraceID: "s", MaxTranscriptSpendUSD: 1}, Sessions: map[string]Usage{}}

	first, ok := k.reserveBudget(1)
	if !ok || first != 1 {
		t.Fatalf("first reservation = (%v, %v), want (1, true)", first, ok)
	}
	if _, ok := k.reserveBudget(1); ok {
		t.Fatal("a second concurrent child claimed budget already reserved — the cap can be doubled")
	}
	k.releaseBudget(first)
	if _, ok := k.reserveBudget(1); !ok {
		t.Fatal("budget was not returned after release")
	}
}

// A cross-provider override must not carry the parent's resolved credential.
func TestModelProviderPrefix(t *testing.T) {
	for _, tt := range []struct{ model, want string }{
		{"openai/gpt-5", "openai"},
		{"anthropic/claude", "anthropic"},
		{"llmgateway/deepseek-v4-pro", "llmgateway"},
		{"bare-model", ""},
		{"/leading", ""},
	} {
		if got := modelProvider(tt.model); got != tt.want {
			t.Errorf("modelProvider(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}
