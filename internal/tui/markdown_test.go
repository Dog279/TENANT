package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func plain(s string) string { return stripANSI(s) }

const doc = "# Title\n\nSome *emphasis*, **strong** and `code` in a sentence long enough to wrap at the width this test renders at.\n\n" +
	"- one\n- two\n  - nested\n\n1. first\n2. second\n\n- [x] done\n- [ ] open\n\n```sh\ntenant backup create\n```\n\n> quoted\n\n" +
	"| A | B |\n|---|---|\n| 1 | 2 |\n\n---\n\n[docs](https://example.com/d) <b>raw</b> [bad](javascript:alert(1))\n"

// Markup is rendered, not shown: no heading hash, emphasis star, backtick or
// table pipe reaches the pane, and raw HTML stays as the text it is.
func TestMarkdownANSI_MarkupGone(t *testing.T) {
	out := plain(markdownANSI(doc, 40, defaultMDTheme()))
	for _, bad := range []string{"# Title", "*emphasis*", "**strong**", "`code`", "| A |", "```", "- [x]", "- [ ]", "javascript:"} {
		if strings.Contains(out, bad) {
			t.Errorf("markup %q leaked into:\n%s", bad, out)
		}
	}
	for _, want := range []string{"Title", "emphasis", "strong", "code", "• one", "• nested", "1. first", "2. second", "✓ done", "○ open",
		"│ sh", "│ tenant backup create", "│ quoted", "│ A", "│ 1", "docs (https://example.com/d)", "<b>raw</b>", "bad"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Every line fits the width, and words are not split to get there.
func TestMarkdownANSI_WrapsAtWordsWithinWidth(t *testing.T) {
	const w = 32
	out := plain(markdownANSI(doc, w, defaultMDTheme()))
	for _, line := range strings.Split(out, "\n") {
		if lipgloss.Width(line) > w {
			t.Errorf("line wider than %d: %q", w, line)
		}
	}
	// "sentence" and "renders" are whole words in the source; a column-chop
	// would have split one of them at this width.
	for _, word := range []string{"sentence", "renders", "emphasis"} {
		if !strings.Contains(out, word) {
			t.Errorf("word %q was split by the wrap:\n%s", word, out)
		}
	}
}

// A code panel keeps its lines (one per source line, tabs widened) and
// hard-breaks a line wider than the pane rather than overflowing it.
func TestMarkdownANSI_CodePanelKeepsLines(t *testing.T) {
	src := "```go\nfunc main() {\n\tfmt.Println(\"a very long string that goes past the pane width for sure\")\n}\n```\n"
	out := plain(markdownANSI(src, 30, defaultMDTheme()))
	lines := strings.Split(out, "\n")
	if lines[0] != "│ go" || lines[1] != "│ func main() {" {
		t.Fatalf("panel head = %q", lines[:2])
	}
	for _, l := range lines {
		if lipgloss.Width(l) > 30 {
			t.Errorf("code line overflows: %q", l)
		}
	}
	if lines[len(lines)-1] != "│ }" {
		t.Errorf("panel tail = %q", lines[len(lines)-1])
	}
}

// Plain prose renders as itself, so an answer with no markup is untouched.
func TestMarkdownANSI_PlainTextUnchanged(t *testing.T) {
	out := plain(markdownANSI("just a sentence", 80, defaultMDTheme()))
	if out != "just a sentence" {
		t.Fatalf("plain text changed: %q", out)
	}
	if markdownANSI("", 80, defaultMDTheme()) != "" {
		t.Fatal("empty source should render empty")
	}
}
