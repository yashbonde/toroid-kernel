package toroid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// writeImg drops a tiny fake image file and returns its absolute path.
func writeImg(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\n fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func partTypes(m fantasy.Message) []fantasy.ContentType {
	var ts []fantasy.ContentType
	for _, p := range m.Content {
		ts = append(ts, p.GetType())
	}
	return ts
}

func TestParseUserMessageInterleaving(t *testing.T) {
	dir := t.TempDir()
	img := writeImg(t, dir, "x.png")

	msg, stored := parseUserMessage("before ![]("+img+") after", dir)

	got := partTypes(msg)
	want := []fantasy.ContentType{fantasy.ContentTypeText, fantasy.ContentTypeFile, fantasy.ContentTypeText}
	if len(got) != len(want) {
		t.Fatalf("part types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("part %d = %v, want %v", i, got[i], want[i])
		}
	}
	if !strings.Contains(stored, img) {
		t.Fatalf("stored prompt %q should contain resolved ref %q", stored, img)
	}
}

func TestParseUserMessageRelativeBecomesAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeImg(t, dir, "img.png")

	// User typed a workDir-relative ref; the persisted prompt must be portable.
	_, stored := parseUserMessage("look ![](img.png)", dir)
	if strings.Contains(stored, "](img.png)") {
		t.Fatalf("relative ref was not rewritten: %q", stored)
	}
	want := tildeOrAbs(filepath.Join(dir, "img.png"))
	if !strings.Contains(stored, want) {
		t.Fatalf("stored %q should contain %q", stored, want)
	}
}

func TestParseUserMessageUnreadableStaysText(t *testing.T) {
	msg, stored := parseUserMessage("see ![](/no/such/file.png) ok", t.TempDir())
	if n := len(msg.Content); n != 1 || msg.Content[0].GetType() != fantasy.ContentTypeText {
		t.Fatalf("expected a single text part, got %d parts %v", n, partTypes(msg))
	}
	if !strings.Contains(stored, "![](/no/such/file.png)") {
		t.Fatalf("unreadable ref should be stored verbatim, got %q", stored)
	}
}

func TestPathToTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	abs := filepath.Join(home, "pics", "a.png")
	if got := pathToTilde(abs); got != "~/pics/a.png" {
		t.Fatalf("pathToTilde(%q) = %q, want ~/pics/a.png", abs, got)
	}
	if got := pathToTilde("/tmp/a.png"); got != "/tmp/a.png" {
		t.Fatalf("outside-home path should be unchanged, got %q", got)
	}
}

func tildeOrAbs(abs string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel := strings.TrimPrefix(abs, home+string(filepath.Separator)); rel != abs {
			return "~/" + rel
		}
	}
	return abs
}
