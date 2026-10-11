package md_test

import (
	"strings"
	"testing"

	"tenant/internal/md"
)

// HTML in the Markdown is text on the page: escaped, never a tag. Script
// schemes in links are dropped.
func TestHTML_RawHTMLIsText(t *testing.T) {
	out := string(md.HTML("hi <script>alert(1)</script> <b>x</b>\n\n<div>block</div>\n\n[l](javascript:alert(1)) ![i](javascript:x)"))
	for _, bad := range []string{"<script", "<b>", "<div>", "javascript:"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q reached the HTML:\n%s", bad, out)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "&lt;b&gt;x&lt;/b&gt;", "&lt;div&gt;block&lt;/div&gt;", `<a href="">l</a>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// The structure a model writes most comes through: headings, lists, code,
// tables and task boxes.
func TestHTML_RendersStructure(t *testing.T) {
	out := string(md.HTML("## H\n\n- a\n- [x] b\n\n```go\nx := 1\n```\n\n| c | d |\n|---|---|\n| 1 | 2 |\n"))
	for _, want := range []string{"<h2>H</h2>", "<ul>", "<li>a</li>", `type="checkbox"`, `<pre><code class="language-go">x := 1`, "<table>", "<th>c</th>", "<td>2</td>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Concurrent renders share one parser and renderer safely.
func TestHTML_Concurrent(t *testing.T) {
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if !strings.Contains(string(md.HTML("# t\n\n- x")), "<h1>t</h1>") {
					t.Error("bad render under concurrency")
					return
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// A fence's language goes through the escaping writer: a quote in it cannot
// break out of the class attribute.
func TestHTML_FenceLanguageIsEscaped(t *testing.T) {
	out := string(md.HTML("```x\"><script>alert(1)</script>\nhi\n```\n"))
	if strings.Contains(out, "<script") || !strings.Contains(out, `class="language-x&quot;&gt;`) {
		t.Fatalf("fence language reached the attribute unescaped:\n%s", out)
	}
}
