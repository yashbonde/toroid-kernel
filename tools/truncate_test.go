package tools

import (
	"strings"
	"testing"
)

func TestTruncateToolOutput(t *testing.T) {
	short := "hello"
	if TruncateToolOutput(short) != short {
		t.Fatal("short string should pass through")
	}
	big := strings.Repeat("x", MaxToolOutputChars+500)
	out := TruncateToolOutput(big)
	if len(out) <= MaxToolOutputChars {
		t.Fatalf("expected truncated body + note, len=%d", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation note: %q", out[len(out)-40:])
	}
	if !strings.HasPrefix(out, strings.Repeat("x", MaxToolOutputChars)) {
		t.Fatal("prefix should be the first MaxToolOutputChars of input")
	}
}
