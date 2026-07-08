package toroid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSkillFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantName string
		wantDesc string
		wantOK   bool
	}{
		{
			name: "valid frontmatter",
			content: "---\n" +
				"name: pdf-export\n" +
				"description: Export a report as a PDF\n" +
				"---\n\nBody text here.",
			wantName: "pdf-export",
			wantDesc: "Export a report as a PDF",
			wantOK:   true,
		},
		{
			name:    "no frontmatter",
			content: "Just a plain file with no header.",
			wantOK:  false,
		},
		{
			name:    "unterminated frontmatter",
			content: "---\nname: foo\ndescription: bar\n",
			wantOK:  false,
		},
		{
			name: "extra keys ignored",
			content: "---\n" +
				"name: x\n" +
				"description: y\n" +
				"author: someone\n" +
				"---\nbody",
			wantName: "x",
			wantDesc: "y",
			wantOK:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name, desc, ok := parseSkillFrontmatter(c.content)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if name != c.wantName || desc != c.wantDesc {
				t.Fatalf("got (%q, %q), want (%q, %q)", name, desc, c.wantName, c.wantDesc)
			}
		})
	}
}

func TestDiscoverSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".toroid", "skills")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	skillBody := "---\nname: greeter\ndescription: Says hello\n---\n\nSay hello to the user."
	if err := os.WriteFile(filepath.Join(dir, "greeter.md"), []byte(skillBody), 0644); err != nil {
		t.Fatal(err)
	}
	// non-.md file should be ignored
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0644); err != nil {
		t.Fatal(err)
	}
	// .md file with no frontmatter should be skipped
	if err := os.WriteFile(filepath.Join(dir, "plain.md"), []byte("no frontmatter here"), 0644); err != nil {
		t.Fatal(err)
	}

	metas, err := discoverSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d skills, want 1: %+v", len(metas), metas)
	}
	if metas[0].Name != "greeter" || metas[0].Description != "Says hello" {
		t.Fatalf("unexpected meta: %+v", metas[0])
	}
	if metas[0].Path != filepath.Join(dir, "greeter.md") {
		t.Fatalf("unexpected path: %s", metas[0].Path)
	}
}
