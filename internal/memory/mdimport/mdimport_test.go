package mdimport

import (
	"strings"
	"testing"
)

func TestParse_ExtractsBulletsAndProse_SkipsScaffolding(t *testing.T) {
	src := `# Memory Index

Some intro prose line that should become a claim.

## Section

- First bullet claim about **Go** preferences.
- Second bullet with a [link](https://example.com/foo) inside it.
* Star bullet using inline ` + "`code`" + ` token.
  - Nested bullet is also a claim.

1. Ordered item one.
2) Ordered item two.

` + "```" + `go
// this code should be skipped entirely
fmt.Println("not a claim")
` + "```" + `

---

> a blockquote line that is meaningful

| col | col |
| --- | --- |
`

	claims := Parse(src)
	texts := make([]string, len(claims))
	for i, c := range claims {
		texts[i] = c.Text
	}
	joined := strings.Join(texts, "\n")

	want := []string{
		"Some intro prose line that should become a claim.",
		"First bullet claim about Go preferences.",
		"Second bullet with a link inside it.",
		"Star bullet using inline code token.",
		"Nested bullet is also a claim.",
		"Ordered item one.",
		"Ordered item two.",
		"a blockquote line that is meaningful",
	}
	if len(claims) != len(want) {
		t.Fatalf("got %d claims, want %d:\n%s", len(claims), len(want), joined)
	}
	for i, w := range want {
		if texts[i] != w {
			t.Errorf("claim %d = %q, want %q", i, texts[i], w)
		}
	}

	// Scaffolding must NOT appear anywhere.
	for _, bad := range []string{"#", "```", "Memory Index", "Section", "fmt.Println", "not a claim", "| col |", "| --- |"} {
		if strings.Contains(joined, bad) {
			t.Errorf("claim text leaked scaffolding %q:\n%s", bad, joined)
		}
	}
}

func TestParse_StripsInlineMarkdown(t *testing.T) {
	cases := map[string]string{
		"- **bold** and *italic* and ~~strike~~":   "bold and italic and strike",
		"- a [display](http://x) link":             "a display link",
		"- inline `code` here":                     "inline code here",
		"- __also bold__ text":                     "also bold text",
		"- autolink <https://example.com> in text": "autolink https://example.com in text",
		"-    extra    whitespace   collapsed":     "extra whitespace collapsed",
	}
	for in, want := range cases {
		got := Parse(in)
		if len(got) != 1 {
			t.Fatalf("Parse(%q) = %d claims, want 1", in, len(got))
		}
		if got[0].Text != want {
			t.Errorf("Parse(%q).Text = %q, want %q", in, got[0].Text, want)
		}
	}
}

func TestParse_DedupsWithinDocument(t *testing.T) {
	src := "- same line\n- Same Line\n- different line"
	claims := Parse(src)
	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2 (case-insensitive dedup)", len(claims))
	}
}

func TestParse_DropsShortNoise(t *testing.T) {
	src := "- ok\n- this is a real claim\n- .\n- >"
	claims := Parse(src)
	// "ok" (2 runes) and the punctuation lines are below the min length.
	if len(claims) != 1 || claims[0].Text != "this is a real claim" {
		t.Fatalf("got %+v, want exactly the real claim", claims)
	}
}

func TestNormalize_IsCaseAndSpaceInsensitive(t *testing.T) {
	if Normalize("  Hello   World ") != Normalize("hello world") {
		t.Error("Normalize should fold case and collapse whitespace")
	}
}
