package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResearchFilename(t *testing.T) {
	name := researchFilename("Compare Hermes vs OpenClaw MCP architectures!")
	if !strings.HasPrefix(name, "research-") || !strings.HasSuffix(name, ".md") {
		t.Fatalf("filename shape wrong: %q", name)
	}
	if strings.ContainsAny(name, " ?!,") {
		t.Fatalf("filename has unsafe chars: %q", name)
	}
	// empty/garbage → still a valid filename.
	if n := researchFilename("???"); !strings.HasPrefix(n, "research-topic-") {
		t.Fatalf("empty-ish question should fall back to 'topic': %q", n)
	}
}

func TestWriteWikiReport_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, err := writeWikiReport(dir, "Test question?", "# Findings\nSome [1] body.\n\n## References\n[1] https://x")
	if err != nil {
		t.Fatalf("writeWikiReport: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("path not in wiki dir: %s", path)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.HasPrefix(got, "# Research: Test question?") {
		t.Fatalf("missing title header: %q", got[:80])
	}
	if !strings.Contains(got, "tenant deep-research") {
		t.Fatalf("missing provenance line: %s", got)
	}
	if !strings.Contains(got, "## References") {
		t.Fatalf("report body lost: %s", got)
	}
	// No wiki dir → clear error.
	if _, err := writeWikiReport("", "q", "r"); err == nil {
		t.Fatal("empty wikiDir should error")
	}
}

// sanitizeReport must collapse degenerate "…" floods and strip leading
// reasoning preambles (the two failure modes we've seen live from aeon-ultimate).
func TestSanitizeReport_CollapsesDotsFlood(t *testing.T) {
	flood := "# Title\n\nReal content.\n" + strings.Repeat("...\n", 200) + "\nMore content."
	got := sanitizeReport(flood)
	if strings.Count(got, "...\n") > 5 {
		t.Fatalf("dot flood not collapsed: %d lines remain", strings.Count(got, "...\n"))
	}
	if !strings.Contains(got, "truncated repeated output") {
		t.Fatalf("missing truncation marker: %q", got)
	}
	if !strings.Contains(got, "# Title") || !strings.Contains(got, "More content") {
		t.Fatalf("collapse ate legitimate content: %q", got)
	}
}

func TestSanitizeReport_StripsReasoningPreamble(t *testing.T) {
	raw := `1. **Identify Finding 1**: blah
2. **Evaluate**: more blah
*Self-Correction:* I should...
*Drafting Response:* okay
Mental Draft: stuff

# Research Report: Openclaw

The actual report content [1].

## Sources
[1] https://example.com`
	got := sanitizeReport(raw)
	if strings.Contains(got, "Self-Correction") || strings.Contains(got, "Mental Draft") {
		t.Fatalf("reasoning preamble not stripped: %q", got)
	}
	if !strings.HasPrefix(got, "# Research Report: Openclaw") {
		t.Fatalf("should start at the first heading; got %q…", got[:80])
	}
}

// Non-reasoning, normal output must pass through untouched (don't over-strip).
func TestSanitizeReport_PreservesNormalOutput(t *testing.T) {
	normal := "# A Real Report\n\nFinding one [1]. Finding two [2].\n\n## Sources\n[1] https://a\n[2] https://b"
	got := sanitizeReport(normal)
	if got != normal {
		t.Fatalf("normal output altered:\n%q\nvs\n%q", got, normal)
	}
}

// normalizeQuestion canonicalizes for cross-cycle dedup (Phase B): case- and
// whitespace-insensitive, so a reflection can't re-ask an already-covered angle.
func TestNormalizeQuestion(t *testing.T) {
	a := normalizeQuestion("  What is   Graphiti?  ")
	b := normalizeQuestion("what is graphiti?")
	if a != b {
		t.Fatalf("normalize mismatch: %q vs %q", a, b)
	}
	if normalizeQuestion("How does X work?") == normalizeQuestion("How does Y work?") {
		t.Fatal("distinct questions must not collide")
	}
	if normalizeQuestion("   ") != "" {
		t.Fatal("blank should normalize to empty (skipped)")
	}
}

// heuristicVague — cheap pre-check for C2's clarifier. Errs toward
// "specific" so we don't burn LLM calls on obvious queries.
func TestHeuristicVague(t *testing.T) {
	vague := []string{
		"nvidia stock", "graphiti", "rust", "what is x?",
		"compare alpha bravo", // 3 words, no caps/digits — vague
	}
	specific := []string{
		"What is the latest closing price of NVDA on the NASDAQ?",      // long
		"compare Graphiti vs OpenClaw MCP architectures",               // 2 proper nouns
		"NVDA Q3 2026 earnings call summary",                           // proper noun + digit
		`research "Hermes 4" on DGX Spark`,                             // quoted phrase
		"summarize golang.org/x/sync/errgroup error semantics in 2026", // has digit
	}
	for _, q := range vague {
		if !heuristicVague(q) {
			t.Errorf("heuristicVague(%q) = false, want true", q)
		}
	}
	for _, q := range specific {
		if heuristicVague(q) {
			t.Errorf("heuristicVague(%q) = true, want false", q)
		}
	}
	// Empty stays "not vague" — we don't prompt on empties (caller rejects).
	if heuristicVague("") {
		t.Error("heuristicVague(empty) should be false (caller handles)")
	}
}

// extractClarifyQuestions pulls 1-2 question-shaped lines from the model's
// response. Strips numbering/bullet prefixes. Requires trailing `?`. Caps at 2.
func TestExtractClarifyQuestions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"clear sentinel", "CLEAR", nil},
		{"plain two questions",
			"What angle — price or strategy?\nWhich timeframe — Q3 only or 12 months?",
			[]string{
				"What angle — price or strategy?",
				"Which timeframe — Q3 only or 12 months?",
			},
		},
		{"numbered prefixes stripped",
			"1. What angle — price or strategy?\n2. Which timeframe — Q3 only or 12 months?",
			[]string{
				"What angle — price or strategy?",
				"Which timeframe — Q3 only or 12 months?",
			},
		},
		{"bulleted prefixes stripped",
			"- What angle — price or strategy?\n* Which timeframe — Q3 or 12 months?",
			[]string{
				"What angle — price or strategy?",
				"Which timeframe — Q3 or 12 months?",
			},
		},
		{"cap at 2",
			"What 1?\nWhat 2?\nWhat 3?\nWhat 4?",
			[]string{"What 1?", "What 2?"},
		},
		{"non-question lines skipped",
			"Here's my thinking:\nWhat angle?\nThat's all.",
			[]string{"What angle?"},
		},
		{"model emitted prose with no question marks → nil",
			"I think this is clear enough to research as-is.",
			nil,
		},
	}
	for _, c := range cases {
		got := extractClarifyQuestions(c.in)
		if len(got) != len(c.want) {
			t.Errorf("[%s] got %d, want %d: %v", c.name, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("[%s][%d] %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// hasClearSentinel must find a standalone CLEAR anywhere in the response
// (not just the prefix) because the reasoning-parser fallback can prepend
// a long thinking block. Must NOT match UNCLEAR, CLEARLY, CLEARED.
func TestHasClearSentinel(t *testing.T) {
	hit := []string{
		"CLEAR",
		"Let me think...\nThe question is specific enough.\nCLEAR",
		"After analysis, my decision: CLEAR",
		"CLEAR: this is well-scoped",
	}
	miss := []string{
		"",
		"UNCLEAR",
		"CLEARLY ambiguous",
		"This is unclear, here are some questions?",
		"Not clear enough — what timeframe?",
		"CLEARED for analysis", // contains the substring but isn't the sentinel
	}
	for _, s := range hit {
		if !hasClearSentinel(s) {
			t.Errorf("hasClearSentinel(%q) = false, want true", s)
		}
	}
	for _, s := range miss {
		if hasClearSentinel(s) {
			t.Errorf("hasClearSentinel(%q) = true, want false", s)
		}
	}
}

// EnrichClarified folds the user's answer into the original question so the
// planner sees a consistent shape regardless of CLI vs TUI entry point.
func TestEnrichClarified(t *testing.T) {
	if got := EnrichClarified("nvidia stock", ""); got != "nvidia stock" {
		t.Errorf("empty answer should pass through: %q", got)
	}
	if got := EnrichClarified("nvidia stock", "latest price + Q3 earnings"); !strings.Contains(got, "nvidia stock") || !strings.Contains(got, "Q3 earnings") {
		t.Errorf("enrichment lost parts: %q", got)
	}
	// Trims whitespace on both sides.
	got := EnrichClarified("  nvidia stock  ", "  Q3 earnings  ")
	if strings.HasPrefix(got, " ") || strings.HasSuffix(got, " ") {
		t.Errorf("not trimmed: %q", got)
	}
}

// ClarifyNeededError carries the questions + original. The TUI uses errors.As
// to detect it; the bridge interface ResearchClarifyError is satisfied via
// ClarifyQuestions() / ClarifyOriginal() methods.
func TestClarifyNeededError(t *testing.T) {
	e := &ClarifyNeededError{
		Question:  "nvidia stock",
		Questions: []string{"What angle?", "Which timeframe?"},
	}
	if !strings.Contains(e.Error(), "clarification needed") {
		t.Errorf("error message wrong: %q", e.Error())
	}
	if !strings.Contains(e.Error(), "2 questions") {
		t.Errorf("error message should mention count: %q", e.Error())
	}
	if got := e.ClarifyQuestions(); len(got) != 2 || got[0] != "What angle?" {
		t.Errorf("ClarifyQuestions wrong: %v", got)
	}
	if got := e.ClarifyOriginal(); got != "nvidia stock" {
		t.Errorf("ClarifyOriginal wrong: %q", got)
	}
	// 1-question form pluralizes correctly.
	one := &ClarifyNeededError{Questions: []string{"q?"}}
	if strings.Contains(one.Error(), "questions") {
		t.Errorf("1-question form should not pluralize: %q", one.Error())
	}
}

// stripToolCallNoise is the orchestrator-level defense against subagents
// leaking tool-call markup as their "report" — the bug that triggered
// `research: no usable findings — The provided findings consist solely of a
// web read tool-call log without any extracted content from the source URL.`
// Must handle every variant we've seen LIVE.
func TestStripToolCallNoise(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain prose unchanged", "Just findings.", "Just findings."},
		{
			"json-shape only",
			`<tool_call>{"name":"x","arguments":{}}</tool_call>`,
			"",
		},
		{
			"hermes xml only",
			"<tool_call>\n<function=web_search>\n<parameter=q>x</parameter>\n</function>\n</tool_call>",
			"",
		},
		{
			"prose surrounding xml — prose survives",
			"Real prose. <function=web_search><parameter=q>x</parameter></function> More prose.",
			"Real prose.  More prose.",
		},
		{
			"malformed live failure: function= without leading <",
			"<tool_call> function=web_read> <parameter>url> https://example.com/x",
			"",
		},
		{
			"gemma fenced",
			"Some intro.\n```tool_code\nweb_search(query=\"x\")\n```\nAfter.",
			"Some intro.\n\nAfter.",
		},
		{
			"real-world combined: prose + leaked markup at end",
			"Found that X is true.\n<tool_call>\n<function=web_navigate>\n<parameter=url>\nhttps://y\n</parameter>\n</function>",
			"Found that X is true.",
		},
	}
	for _, c := range cases {
		got := stripToolCallNoise(c.in)
		if got != c.want {
			t.Errorf("[%s]\nin:   %q\ngot:  %q\nwant: %q", c.name, c.in, got, c.want)
		}
	}
}

// looksLikeQuestion filters CoT smuggled in as bulleted "sub-questions" —
// live trigger: a reflect() response contained "- *Correction:* The prompt
// says..." which became a researcher's task and produced garbage findings.
func TestLooksLikeQuestion_RejectsReasoningCoT(t *testing.T) {
	reject := []string{
		"*Correction:* The prompt says I need to dig deeper",
		"*Self-Correction* this isn't quite right",
		"Self-Correction: my plan was off",
		"Mental Draft: how about we instead...",
		"*Drafting Response:* let me think",
		"Let me think about this differently",
		"I need to dig deeper into the original question",
		"Wait, that doesn't match what was asked",
		"Actually, I should reconsider the framing",
		"The prompt says I should focus on X but...",
		strings.Repeat("Long reasoning paragraph that runs on. ", 20), // >300 chars
	}
	for _, item := range reject {
		if looksLikeQuestion(item) {
			t.Errorf("should REJECT reasoning text: %q…", clip(item, 60))
		}
	}
	accept := []string{
		"What is Hermes 4?",
		"How does vLLM optimize PagedAttention on Blackwell?",
		"Compare DGX Spark to AWS B-series for Hermes inference",
		"Find benchmarks of Hermes 4 NVFP4 vs FP8",
		// Capitalized normal sentence — accepted (not all questions end in ?)
		"Latest pricing for AWS B200 instances",
	}
	for _, item := range accept {
		if !looksLikeQuestion(item) {
			t.Errorf("should ACCEPT real question: %q", item)
		}
	}
}

// parseNumberedList must apply the question filter — a bulleted CoT item
// gets dropped even though the regex matched.
func TestParseNumberedList_FiltersCoTItems(t *testing.T) {
	in := "1. What is Hermes 4?\n" +
		"2. *Correction:* The prompt says I should approach this differently and dig deeper\n" +
		"3. How does DGX Spark handle FP4?\n"
	got := parseNumberedList(in, 5)
	if len(got) != 2 {
		t.Fatalf("CoT item not filtered: got %d, want 2 (%v)", len(got), got)
	}
	if got[0] != "What is Hermes 4?" || got[1] != "How does DGX Spark handle FP4?" {
		t.Errorf("wrong items survived: %v", got)
	}
}

func TestParseNumberedList(t *testing.T) {
	in := "Here is the plan:\n1. What is X?\n2) How does Y work?\n- A bulleted one\n* star item\n3. capped\n"
	got := parseNumberedList(in, 3)
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3 (capped): %v", len(got), got)
	}
	if got[0] != "What is X?" || got[1] != "How does Y work?" || got[2] != "A bulleted one" {
		t.Fatalf("parsed wrong: %v", got)
	}
	// Prose with no list → empty.
	if n := parseNumberedList("just some prose, no list here", 5); len(n) != 0 {
		t.Fatalf("prose should yield no items, got %v", n)
	}
}

func TestSplitSources(t *testing.T) {
	rep := "Finding text with [1] and [2].\n\n## Sources\n[1] https://a.com/x\n[2] https://b.com/y"
	body, srcs := splitSources(rep)
	if strings.Contains(body, "Sources") {
		t.Fatalf("body should exclude the Sources block: %q", body)
	}
	if srcs[1] != "https://a.com/x" || srcs[2] != "https://b.com/y" {
		t.Fatalf("sources parsed wrong: %v", srcs)
	}
	// No sources block → whole body, empty map.
	b2, s2 := splitSources("no citations here")
	if b2 != "no citations here" || len(s2) != 0 {
		t.Fatalf("no-sources case wrong: %q %v", b2, s2)
	}
}

// collapseCitations dedups URLs into one global numbering, rewrites each
// finding's local markers, skips errored/empty agents, and builds References.
func TestCollapseCitations(t *testing.T) {
	results := []TeamResult{
		{Status: "done", Role: "researcher", Result: "Go is great [1]. Also fast [2].\n## Sources\n[1] https://go.dev\n[2] https://example.com"},
		{Status: "done", Role: "researcher", Result: "Goroutines are cheap [1].\n## Sources\n[1] https://go.dev"}, // shares go.dev
		{Status: "error", Role: "researcher", Result: "error: boom"},                                              // skipped
		{Status: "done", Role: "researcher", Result: "(no result)"},                                               // skipped
	}
	combined, refs := collapseCitations(results)

	// go.dev seen first → global [1]; example.com → global [2].
	wantRefs := "## References\n[1] https://go.dev\n[2] https://example.com"
	if refs != wantRefs {
		t.Fatalf("references =\n%q\nwant\n%q", refs, wantRefs)
	}
	// Finding 2's local [1] (go.dev) must renumber to global [1] (same).
	if !strings.Contains(combined, "Goroutines are cheap [1].") {
		t.Fatalf("finding 2 marker not preserved/renumbered: %q", combined)
	}
	// Finding 1 keeps [1]/[2] (go.dev=1, example.com=2).
	if !strings.Contains(combined, "Go is great [1]. Also fast [2].") {
		t.Fatalf("finding 1 markers wrong: %q", combined)
	}
	// Two findings rendered, errored/empty skipped.
	if strings.Count(combined, "### Finding ") != 2 {
		t.Fatalf("expected 2 findings, got: %q", combined)
	}
	if strings.Contains(combined, "boom") || strings.Contains(combined, "no result") {
		t.Fatalf("errored/empty agent leaked into combined: %q", combined)
	}
}

// C1 — internal sources are first-class citations alongside http(s). The
// Sources-line parser accepts wiki:<file>, memory:<id>, and any RFC-3986
// scheme; the dedup keys naturally on the full URI string.
func TestSplitSources_InternalSchemes(t *testing.T) {
	rep := `Finding text mixing [1] web and [2] wiki sources, plus [3] memory.

## Sources
[1] https://example.com/page
[2] wiki:notes/research/foo.md
[3] memory:fact-abc123`
	body, srcs := splitSources(rep)
	if strings.Contains(body, "## Sources") {
		t.Fatalf("body should exclude the Sources block: %q", body)
	}
	if srcs[1] != "https://example.com/page" {
		t.Fatalf("web src wrong: %q", srcs[1])
	}
	if srcs[2] != "wiki:notes/research/foo.md" {
		t.Fatalf("wiki src wrong (regex didn't match the scheme): %q", srcs[2])
	}
	if srcs[3] != "memory:fact-abc123" {
		t.Fatalf("memory src wrong: %q", srcs[3])
	}
}

// Mixed web + wiki across two researchers: dedup by full URI (wiki and web
// can't collide because the URI prefix differs), global numbering preserved.
func TestCollapseCitations_MixedSchemes(t *testing.T) {
	results := []TeamResult{
		{Status: "done", Role: "researcher", Result: "Web claim [1]. Internal note [2].\n## Sources\n[1] https://go.dev\n[2] wiki:notes/go.md"},
		{Status: "done", Role: "researcher", Result: "Same wiki note [1]. Different web [2].\n## Sources\n[1] wiki:notes/go.md\n[2] https://example.com"},
	}
	combined, refs := collapseCitations(results)

	// Order of first sight: https://go.dev (1), wiki:notes/go.md (2), https://example.com (3).
	want := "## References\n[1] https://go.dev\n[2] wiki:notes/go.md\n[3] https://example.com"
	if refs != want {
		t.Fatalf("references mixed-scheme wrong:\n got %q\nwant %q", refs, want)
	}
	// Second researcher's local [1] (wiki) must renumber to global [2].
	if !strings.Contains(combined, "Same wiki note [2].") {
		t.Fatalf("wiki marker not renumbered to global [2]: %q", combined)
	}
	if !strings.Contains(combined, "Different web [3].") {
		t.Fatalf("web marker not renumbered to global [3]: %q", combined)
	}
}

// Edge case: a "Note:" line in prose inside the Sources block must NOT be
// misparsed as a source entry (no leading [n], no scheme:// — but does have
// a colon). Defensive coverage so the regex tightening doesn't flip false.
func TestSplitSources_IgnoresProseLines(t *testing.T) {
	rep := `Body text.

## Sources
[1] wiki:foo.md
Note: this is a prose annotation, not a source.
[2] https://example.com`
	_, srcs := splitSources(rep)
	if len(srcs) != 2 {
		t.Fatalf("prose line bled into srcs map: %v", srcs)
	}
	if srcs[1] != "wiki:foo.md" || srcs[2] != "https://example.com" {
		t.Fatalf("real sources wrong: %v", srcs)
	}
}

// A finding citing a marker with no matching Sources entry leaves it as-is
// (no panic, no bogus reference).
func TestCollapseCitations_UnmappedMarker(t *testing.T) {
	results := []TeamResult{
		{Status: "done", Role: "researcher", Result: "Claim with a dangling [5] marker and no sources block."},
	}
	combined, refs := collapseCitations(results)
	if !strings.Contains(combined, "[5]") {
		t.Fatalf("unmapped marker should be left intact: %q", combined)
	}
	if refs != "" {
		t.Fatalf("no sources → no references, got %q", refs)
	}
}
