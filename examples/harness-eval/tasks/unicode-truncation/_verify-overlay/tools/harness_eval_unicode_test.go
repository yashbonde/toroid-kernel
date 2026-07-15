package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHarnessTruncateToolOutputPreservesUTF8(t *testing.T) {
	in := strings.Repeat("a", MaxToolOutputChars-1) + "🙂" + strings.Repeat("界", 100)
	got := TruncateToolOutput(nil, in)
	if !utf8.ValidString(got) {
		t.Fatal("truncated output is not valid UTF-8")
	}
	i := strings.Index(got, "\n… [truncated")
	if i < 0 {
		t.Fatalf("missing truncation diagnostic: %q", got[len(got)-80:])
	}
	if i > MaxToolOutputChars {
		t.Fatalf("retained prefix is %d bytes, exceeds cap %d", i, MaxToolOutputChars)
	}
	if got[:i] != strings.Repeat("a", MaxToolOutputChars-1) {
		t.Fatalf("unexpected retained prefix length %d", i)
	}
}

func TestHarnessTruncateToolOutputASCIIBoundary(t *testing.T) {
	in := strings.Repeat("x", MaxToolOutputChars+1)
	got := TruncateToolOutput(nil, in)
	if !strings.HasPrefix(got, strings.Repeat("x", MaxToolOutputChars)) {
		t.Fatal("ASCII truncation no longer retains the full byte budget")
	}
}
