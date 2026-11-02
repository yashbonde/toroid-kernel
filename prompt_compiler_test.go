package toroid

import "testing"

func TestSystemPromptStablePrefixIgnoresRuntimeContext(t *testing.T) {
	prefixA, suffixA := buildSystemPrompt("/workspace/a", nil, false, false, false, "model-a", "", 100_000, 10_000, nil)
	prefixB, suffixB := buildSystemPrompt("/workspace/b", []SkillMeta{{Name: "review", Path: "/skills/review", Description: "Review code"}}, true, true, false, "model-b", "small", 200_000, 20_000, nil)

	if prefixA != prefixB {
		t.Fatal("runtime context changed the invariant cache prefix")
	}
	if suffixA == suffixB {
		t.Fatal("different runtime context produced identical suffixes")
	}
}

func TestSystemPromptSortsSkillsDeterministically(t *testing.T) {
	a := SkillMeta{Name: "alpha", Path: "/skills/alpha", Description: "Alpha"}
	z := SkillMeta{Name: "zeta", Path: "/skills/zeta", Description: "Zeta"}
	_, suffixA := buildSystemPrompt("/workspace", []SkillMeta{z, a}, false, false, false, "model", "", 100_000, 10_000, nil)
	_, suffixB := buildSystemPrompt("/workspace", []SkillMeta{a, z}, false, false, false, "model", "", 100_000, 10_000, nil)

	if suffixA != suffixB {
		t.Fatal("skill discovery order changed the system prompt")
	}
}
