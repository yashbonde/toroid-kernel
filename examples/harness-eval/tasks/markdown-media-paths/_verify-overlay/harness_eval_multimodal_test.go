package toroid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yashbonde/toroid-kernel/llm"
)

func TestHarnessMarkdownEscapedParenMediaPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screen(1).png")
	if err := os.WriteFile(path, []byte("png-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, stored, warnings := parseUserMessage(`before ![shot](screen\(1\).png) after`, dir, ResolveModel("llmgateway/claude-sonnet-4-5"))
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(msg.Parts) != 3 {
		t.Fatalf("parts = %#v, want text/file/text", msg.Parts)
	}
	fp, ok := msg.Parts[1].(llm.FilePart)
	if !ok {
		t.Fatalf("middle part = %T, want llm.FilePart", msg.Parts[1])
	}
	if fp.Filename != "screen(1).png" {
		t.Fatalf("filename = %q", fp.Filename)
	}
	if stored == `before ![shot](screen\(1\).png) after` {
		t.Fatal("stored prompt retained escaped relative path")
	}
}

func TestHarnessMarkdownAngleDestinationWithSpace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "screen shot.png"), []byte("png-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, _, warnings := parseUserMessage(`![shot](<screen shot.png>)`, dir, ResolveModel("llmgateway/claude-sonnet-4-5"))
	if len(warnings) != 0 || len(msg.Parts) != 1 {
		t.Fatalf("parts=%#v warnings=%v", msg.Parts, warnings)
	}
	if _, ok := msg.Parts[0].(llm.FilePart); !ok {
		t.Fatalf("part = %T, want llm.FilePart", msg.Parts[0])
	}
}
