package agent

import (
	"strings"
	"testing"

	"tenant/internal/memory/working"
)

// looksLikeToolCallMarkup must catch the live failure modes — aeon-ultimate
// emitting <tool_call>... blocks in tools-off synthesis. False positives on
// prose mentioning these markers are acceptable IF the markers are dominant.
func TestLooksLikeToolCallMarkup(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"plain prose", "Apple's revenue grew 12% YoY in Q4.", false},
		{"starts with tool_call", "<tool_call>{\"name\":\"x\"}</tool_call>", true},
		{"starts with function= XML", "<function=web_search><parameter=q>x</parameter></function>", true},
		{"starts with parameter=", "<parameter=url>https://x</parameter>", true},
		{"starts with gemma fenced", "```tool_code\nweb_search(q=\"x\")\n```", true},
		{"leading whitespace + tool_call", "\n\n  <tool_call>...</tool_call>", true},
		{"prose mentioning the markers in passing — accept", "We used <tool_call> tags in the API.", false},
		{
			"live failure: short malformed tool call body",
			"<tool_call>\n<function=web_navigate>\n<parameter=url>\nhttps://x",
			true,
		},
	}
	for _, c := range cases {
		got := looksLikeToolCallMarkup(c.in)
		if got != c.want {
			t.Errorf("[%s] looksLikeToolCallMarkup(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// summarizeToolMemory must extract `tool` role messages as a bulleted dump
// — used as the last-resort answer when the model refuses to write one.
// Caps per-message length so a 20K web_read doesn't dominate. Caps total
// count.
func TestSummarizeToolMemory(t *testing.T) {
	w := []working.Message{
		{Role: "user", Content: "research question"},                                 // skipped
		{Role: "tool", Content: "web_search results: 5 hits about NVDA Q4 earnings"}, // kept
		{Role: "assistant", Content: "I will navigate..."},                           // skipped
		{Role: "tool", Content: "page text: NVDA closed at $219.51..."},              // kept
		{Role: "tool", Content: ""},                                                  // skipped (empty)
	}
	got := summarizeToolMemory(w)
	if !strings.Contains(got, "Tool results gathered") {
		t.Errorf("header missing: %q", got)
	}
	if !strings.Contains(got, "NVDA Q4 earnings") || !strings.Contains(got, "NVDA closed at $219.51") {
		t.Errorf("tool bodies lost: %q", got)
	}
	if strings.Contains(got, "research question") || strings.Contains(got, "I will navigate") {
		t.Errorf("non-tool roles leaked: %q", got)
	}

	// Per-message length cap: an oversized tool result is truncated.
	big := strings.Repeat("x", 5000)
	w = []working.Message{{Role: "tool", Content: big}}
	got = summarizeToolMemory(w)
	if strings.Contains(got, big) {
		t.Error("oversized tool message should be truncated")
	}
	if !strings.Contains(got, "…") {
		t.Errorf("truncation marker missing: %q", got)
	}

	// Empty tool memory → empty string (caller falls through to original empty path).
	if got := summarizeToolMemory(nil); got != "" {
		t.Errorf("nil msgs should return empty, got %q", got)
	}
	if got := summarizeToolMemory([]working.Message{{Role: "user", Content: "q"}}); got != "" {
		t.Errorf("no tool msgs should return empty, got %q", got)
	}
}
