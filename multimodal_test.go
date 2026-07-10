package toroid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// writeTempImage creates a fake image file of n bytes with a media extension.
// MediaType keys off the extension, so the bytes need not be a real PNG.
func writeTempImage(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hasImagePart(msg fantasy.Message) bool {
	for _, part := range msg.Content {
		if _, ok := part.(fantasy.FilePart); ok {
			return true
		}
	}
	return false
}

func TestParseUserMessageVisionModelInlinesImage(t *testing.T) {
	img := writeTempImage(t, 1024)
	vision := ResolveModel("llmgateway/claude-sonnet-4-5") // supports image
	msg, _, warns := parseUserMessage("look ![shot]("+img+")", "", vision)

	if !hasImagePart(msg) {
		t.Error("expected image to be inlined as a FilePart on a vision model")
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings, got %v", warns)
	}
}

func TestParseUserMessageTextOnlyModelWarnsInsteadOfDropping(t *testing.T) {
	img := writeTempImage(t, 1024)
	textOnly := ResolveModel("llmgateway/some-text-model") // unknown -> text-only
	msg, _, warns := parseUserMessage("look ![shot]("+img+")", "", textOnly)

	if hasImagePart(msg) {
		t.Error("text-only model should not receive an image part")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "does not accept image") {
		t.Errorf("expected a capability warning, got %v", warns)
	}
}

func TestParseUserMessageOversizedMediaRejected(t *testing.T) {
	big := writeTempImage(t, maxInlineMediaBytes+1)
	vision := ResolveModel("llmgateway/claude-sonnet-4-5")
	msg, _, warns := parseUserMessage("big ![shot]("+big+")", "", vision)

	if hasImagePart(msg) {
		t.Error("oversized media must not be inlined")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "inline cap") {
		t.Errorf("expected an oversize warning, got %v", warns)
	}
}

func TestParseUserMessageNonMediaLinkIsSilent(t *testing.T) {
	vision := ResolveModel("llmgateway/claude-sonnet-4-5")
	// A .txt ref is not media: it stays as text with no warning noise.
	_, _, warns := parseUserMessage("see ![doc](notes.txt)", "", vision)
	if len(warns) != 0 {
		t.Errorf("non-media ref should not warn, got %v", warns)
	}
}
