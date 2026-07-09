package toroid

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
)

func TestBuildSystemPromptIncludesSmallerModel(t *testing.T) {
	with, err := buildSystemPrompt("/tmp/work", nil, "anthropic/claude-sonnet-4-5", "anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with, "claude-haiku-4-5") {
		t.Fatalf("expected SmallerModel in prompt, got:\n%s", with)
	}
	if !strings.Contains(with, "Model routing") {
		t.Fatalf("expected Model routing section, got:\n%s", with)
	}

	without, err := buildSystemPrompt("/tmp/work", nil, "anthropic/claude-sonnet-4-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "Model routing") {
		t.Fatalf("did not expect Model routing when SmallerModel empty")
	}
}

func TestPreparePromptCacheStep(t *testing.T) {
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("u1"),
		fantasy.NewUserMessage("u2"),
		fantasy.NewUserMessage("u3"),
	}
	_, prepared, err := preparePromptCacheStep(context.Background(), fantasy.PrepareStepFunctionOptions{
		Messages: msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Messages) != 4 {
		t.Fatalf("len=%d", len(prepared.Messages))
	}
	// System must be cached.
	if prepared.Messages[0].ProviderOptions[anthropic.Name] == nil {
		t.Fatal("expected cache options on system message")
	}
	// Last two messages must be cached.
	if prepared.Messages[2].ProviderOptions[anthropic.Name] == nil {
		t.Fatal("expected cache on messages[2]")
	}
	if prepared.Messages[3].ProviderOptions[anthropic.Name] == nil {
		t.Fatal("expected cache on messages[3]")
	}
	// Middle non-system (u1) should not keep a breakpoint when not in last-2.
	// Index 1 is "u1"; with last-2 covering indices 2 and 3 only, index 1 is clear
	// unless it was system. preparePromptCache clears then re-applies.
	if prepared.Messages[1].ProviderOptions[anthropic.Name] != nil {
		t.Fatal("did not expect cache on messages[1]")
	}
}

func TestPromptCacheEnabledDefault(t *testing.T) {
	if !promptCacheEnabled(Config{}) {
		t.Fatal("nil PromptCache should default to enabled")
	}
	off := false
	if promptCacheEnabled(Config{PromptCache: &off}) {
		t.Fatal("explicit false should disable")
	}
	on := true
	if !promptCacheEnabled(Config{PromptCache: &on}) {
		t.Fatal("explicit true should enable")
	}
}

func TestCheapModelHelpers(t *testing.T) {
	k := &Kernel{Cfg: Config{Model: "primary", SmallerModel: "cheap"}}
	if k.cheapModelID() != "cheap" {
		t.Fatalf("got %q", k.cheapModelID())
	}
	k2 := &Kernel{Cfg: Config{Model: "primary"}}
	if k2.cheapModelID() != "primary" {
		t.Fatalf("got %q", k2.cheapModelID())
	}
}
