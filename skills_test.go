package toroid

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestDiscoverNestedAgentSkill exercises the ~/.agents/skills nested layout:
// each skill is a directory containing a SKILL.md. It points discoverSkillsFrom
// at a temp dir so we avoid touching a real home dir.
func TestDiscoverNestedAgentSkill(t *testing.T) {
	tmp := t.TempDir()

	// Flat toroid-style skill (existing behavior preserved).
	if err := os.MkdirAll(filepath.Join(tmp, "toroid"), 0755); err != nil {
		t.Fatal(err)
	}
	toroidSkill := filepath.Join(tmp, "toroid", "flat.md")
	writeSkillFile(t, toroidSkill, "---\nname: flatskill\ndescription: a flat skill\n---\n")

	// Nested agents-style skill, e.g. ~/.agents/skills/aidocs/SKILL.md.
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "aidocs"), 0755); err != nil {
		t.Fatal(err)
	}
	nestedSkill := filepath.Join(tmp, "agents", "aidocs", "SKILL.md")
	writeSkillFile(t, nestedSkill, "---\nname: aidocs\ndescription: a nested skill\n---\n")

	metas := discoverSkillsFrom([]string{filepath.Join(tmp, "toroid"), filepath.Join(tmp, "agents")})

	want := []SkillMeta{
		{Name: "flatskill", Description: "a flat skill", Path: toroidSkill},
		{Name: "aidocs", Description: "a nested skill", Path: nestedSkill},
	}
	if !reflect.DeepEqual(metas, want) {
		t.Fatalf("discoverSkillsFrom = %v, want %v", metas, want)
	}
}

// TestDiscoverAgentsSkillByDefault checks that a nested ~/.agents skill is
// discovered even when the toroid dir contributes no flat skills, so the skill
// tool still gets registered.
func TestDiscoverAgentsSkillByDefault(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "toroid"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "aisites"), 0755); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, filepath.Join(tmp, "agents", "aisites", "SKILL.md"), "---\nname: aisites\ndescription: a nested skill\n---\n")

	metas := discoverSkillsFrom([]string{filepath.Join(tmp, "toroid"), filepath.Join(tmp, "agents")})
	if len(metas) != 1 || metas[0].Name != "aisites" {
		t.Fatalf("discoverSkillsFrom = %v, want one nested skill", metas)
	}
}

// TestDiscoverSkillsDedup verifies the toroid dir wins when the same name
// appears in both sources.
func TestDiscoverSkillsDedup(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "toroid"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "dup"), 0755); err != nil {
		t.Fatal(err)
	}
	toroidPath := filepath.Join(tmp, "toroid", "dup.md")
	writeSkillFile(t, toroidPath, "---\nname: dup\ndescription: toroid version\n---\n")
	writeSkillFile(t, filepath.Join(tmp, "agents", "dup", "SKILL.md"), "---\nname: dup\ndescription: agents version\n---\n")

	metas := discoverSkillsFrom([]string{filepath.Join(tmp, "toroid"), filepath.Join(tmp, "agents")})
	if len(metas) != 1 || metas[0].Path != toroidPath || metas[0].Description != "toroid version" {
		t.Fatalf("discoverSkillsFrom = %v, want the toroid-dir dup winning", metas)
	}
}

// TestDiscoverSkillsSkipsMalformedAndMissing verifies missing/unreadable dirs
// and malformed skill files are skipped silently rather than resetting startup.
func TestDiscoverSkillsSkipsMalformedAndMissing(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "does-not-exist")

	// Missing source dirs are skipped silently.
	for _, dirs := range [][]string{{missing}, {filepath.Join(tmp, "-agents"), missing}} {
		if metas := discoverSkillsFrom(dirs); len(metas) != 0 {
			t.Fatalf("discoverSkillsFrom(%v) = %v, want none", dirs, metas)
		}
	}

	// A malformed skill (no frontmatter) is skipped while a valid one is kept.
	if err := os.MkdirAll(filepath.Join(tmp, "skills"), 0755); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, filepath.Join(tmp, "skills", "bad.md"), "no frontmatter here\n")
	writeSkillFile(t, filepath.Join(tmp, "skills", "valid.md"), "---\nname: valid\ndescription: ok\n---\n")
	metas := discoverSkillsFrom([]string{filepath.Join(tmp, "skills")})
	if len(metas) != 1 || metas[0].Name != "valid" {
		t.Fatalf("discoverSkillsFrom = %v, want only the valid skill", metas)
	}
}

func writeSkillFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
