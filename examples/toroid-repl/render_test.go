package main

import (
	"strings"
	"testing"
)

func TestRenderMarkdownBasics(t *testing.T) {
	src := "# Title\n\nSome **bold** and `code` and a [link](http://x).\n\n" +
		"- one\n- two\n\n```go\nfmt.Println(\"hi\")\n```\n"
	out := renderMarkdown(src, 80)

	for _, want := range []string{
		aHeading + "# Title",          // heading styled
		aBold + "bold",                // bold inline
		aCodeBG,                       // inline code background
		"http://x",                    // link target kept
		aCyan + "• ",                  // bullet marker
		"┌─ go",                       // fenced block header with language
		"fmt.Println",                 // code passed through verbatim
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n---\n%s", want, out)
		}
	}
}

func TestTrimOneLine(t *testing.T) {
	got := trimOneLine("hello world this is long", 8)
	if !strings.HasPrefix(got, "hello wo") {
		t.Errorf("expected truncation, got %q", got)
	}
	multi := trimOneLine("first\nsecond\nthird", 100)
	if !strings.Contains(multi, "first") || !strings.Contains(multi, "+2 lines") {
		t.Errorf("expected first line + line count, got %q", multi)
	}
}
