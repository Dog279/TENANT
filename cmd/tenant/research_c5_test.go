package main

import (
	"context"
	"strings"
	"testing"
)

// TestHasReflectDoneSentinel — drift guard for the standalone-DONE
// matcher. The whole point of replacing parseNumberedList's silent
// empty-return with an explicit sentinel was so a thinking model can
// emit reasoning + "DONE" without being mis-parsed. The matcher must
// (a) treat "DONE" as a standalone token on its own line and
// (b) refuse substring matches like WELLDONE/DONELY that would
// false-positive a continuing investigation as finished.
func TestHasReflectDoneSentinel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"bare", "DONE", true},
		{"surrounded by whitespace", "   DONE   ", true},
		{"trailing period", "DONE.", true},
		{"trailing bang", "DONE!", true},
		{"markdown emphasis", "**DONE**", true},
		{"bracketed", "[DONE]", true},
		{"lowercase", "done", true},
		{"mixed case", "Done.", true},
		// substrings: must NOT fire
		{"WELLDONE substring", "WELLDONE", false},
		{"DONELY substring", "DONELY", false},
		// natural sentence containing DONE — must NOT fire
		{"buried in sentence", "The investigation is DONE", false},
		// numbered-list item — must NOT fire (caller will parse the list)
		{"numbered list item", "1. DONE", false},
		// thinking-model preamble, sentinel on its own line — MUST fire
		{"after reasoning preamble", "Looking through findings, all key angles answered.\n\nDONE", true},
		// gaps only — must NOT fire
		{"gaps without sentinel", "1. What about X?\n2. What about Y?", false},
		// sentinel + accidental list — sentinel wins
		{"sentinel plus stray list", "DONE\n\n1. (was going to ask but no need)", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasReflectDoneSentinel(c.in); got != c.want {
				t.Errorf("hasReflectDoneSentinel(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// fakeEmbedder returns vectors from a name→vec map. Tests build
// hand-crafted embeddings so cosine outcomes are deterministic.
type fakeEmbedder struct {
	byText map[string][]float32
	err    error
	calls  int
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := f.byText[t]
		if !ok {
			// default: orthogonal junk so it doesn't accidentally near-match
			v = []float32{0, 0, 1}
		}
		out[i] = v
	}
	return out, nil
}

// TestRerankAndDedup_NilEmbedderPassthrough — without an embedder we
// MUST not touch the slice (and must not panic). Best-effort guarantee.
func TestRerankAndDedup_NilEmbedderPassthrough(t *testing.T) {
	in := []TeamResult{
		{ID: "a1", Status: "done", Result: "x"},
		{ID: "a2", Status: "done", Result: "y"},
	}
	got := rerankAndDedupFindings(context.Background(), nil, "q", in, nil)
	if len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a2" {
		t.Errorf("passthrough broken: %+v", got)
	}
}

// TestRerankAndDedup_TooFewFindings — single done finding can't be
// reranked or deduplicated. Must return unchanged without calling Embed.
func TestRerankAndDedup_TooFewFindings(t *testing.T) {
	emb := &fakeEmbedder{byText: map[string][]float32{}}
	in := []TeamResult{
		{ID: "a1", Status: "done", Result: "only one"},
		{ID: "a2", Status: "error", Result: "boom"},
	}
	got := rerankAndDedupFindings(context.Background(), emb, "q", in, nil)
	if len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a2" {
		t.Errorf("expected unchanged, got %+v", got)
	}
	if emb.calls != 0 {
		t.Errorf("should not have called Embed for 1 done finding; calls=%d", emb.calls)
	}
}

// TestRerankAndDedup_RanksByRelevance — finding closer to the question
// vector floats to the top of the returned slice. citation collapse
// uses this order to assign global [1], [2], ...
func TestRerankAndDedup_RanksByRelevance(t *testing.T) {
	emb := &fakeEmbedder{byText: map[string][]float32{
		"q":          {1, 0, 0},
		"low-match":  {0, 1, 0}, // cos=0 to q
		"high-match": {1, 0, 0}, // cos=1 to q
	}}
	in := []TeamResult{
		{ID: "low", Status: "done", Result: "low-match"},
		{ID: "high", Status: "done", Result: "high-match"},
	}
	got := rerankAndDedupFindings(context.Background(), emb, "q", in, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
	if got[0].ID != "high" || got[1].ID != "low" {
		t.Errorf("rerank order wrong: got %s,%s want high,low", got[0].ID, got[1].ID)
	}
}

// TestRerankAndDedup_DropsNearDuplicates — two findings with sim ≥ 0.92
// to each other → the lower-ranked one is dropped from synthesis input.
// The dropped report still exists upstream in run.AppendFinding (not
// tested here — that's a different call site, audited in C3 tests).
func TestRerankAndDedup_DropsNearDuplicates(t *testing.T) {
	// Use parallel vectors so cosine is exactly 1 between dup pair, and
	// a distinct vector for the third report.
	emb := &fakeEmbedder{byText: map[string][]float32{
		"q":        {1, 0, 0},
		"dup-body": {1, 0, 0},
		"distinct": {0, 1, 0},
	}}
	in := []TeamResult{
		{ID: "first-dup", Status: "done", Result: "dup-body"},
		{ID: "second-dup", Status: "done", Result: "dup-body"},
		{ID: "distinct", Status: "done", Result: "distinct"},
	}
	var logged []string
	say := func(format string, args ...any) {
		// crude format — enough to confirm we surfaced the drop
		logged = append(logged, format)
	}
	got := rerankAndDedupFindings(context.Background(), emb, "q", in, say)
	// One of the dups must be gone; "distinct" must survive.
	gotIDs := map[string]bool{}
	for _, r := range got {
		gotIDs[r.ID] = true
	}
	if !gotIDs["distinct"] {
		t.Errorf("distinct should survive: %+v", got)
	}
	dupCount := 0
	if gotIDs["first-dup"] {
		dupCount++
	}
	if gotIDs["second-dup"] {
		dupCount++
	}
	if dupCount != 1 {
		t.Errorf("exactly one dup should remain, got %d; result=%+v", dupCount, got)
	}
	// And we must have logged the drop so operators see why.
	sawDropLog := false
	for _, m := range logged {
		if strings.Contains(m, "near-duplicate") {
			sawDropLog = true
			break
		}
	}
	if !sawDropLog {
		t.Errorf("expected a near-duplicate log line, got %v", logged)
	}
}

// TestRerankAndDedup_EmbedErrorPassthrough — Embed() returning an error
// (e.g. the embed backend is down) MUST not lose findings. Best-effort
// rerank/dedup never blocks synthesis.
func TestRerankAndDedup_EmbedErrorPassthrough(t *testing.T) {
	emb := &fakeEmbedder{err: context.DeadlineExceeded}
	in := []TeamResult{
		{ID: "a1", Status: "done", Result: "body 1"},
		{ID: "a2", Status: "done", Result: "body 2"},
	}
	got := rerankAndDedupFindings(context.Background(), emb, "q", in, nil)
	if len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a2" {
		t.Errorf("error should pass through unchanged: %+v", got)
	}
}

// TestRerankAndDedup_PreservesFailureTail — errored / empty findings
// stay in the slice (tail-appended) so any audit-shape consumer sees
// every original entry. collapseCitations skips them downstream.
func TestRerankAndDedup_PreservesFailureTail(t *testing.T) {
	emb := &fakeEmbedder{byText: map[string][]float32{
		"q":   {1, 0, 0},
		"a-r": {1, 0, 0},
		"b-r": {0, 1, 0},
	}}
	in := []TeamResult{
		{ID: "ok-a", Status: "done", Result: "a-r"},
		{ID: "errored", Status: "error", Result: "stack trace..."},
		{ID: "ok-b", Status: "done", Result: "b-r"},
		{ID: "empty", Status: "done", Result: ""},
	}
	got := rerankAndDedupFindings(context.Background(), emb, "q", in, nil)
	if len(got) != 4 {
		t.Fatalf("all 4 results must be preserved (kept + failures appended), got %d: %+v", len(got), got)
	}
	// First two are the kept done findings, by relevance to q (a-r > b-r).
	if got[0].ID != "ok-a" || got[1].ID != "ok-b" {
		t.Errorf("kept-order wrong: %s,%s want ok-a,ok-b", got[0].ID, got[1].ID)
	}
	// Last two are the failures, in original order.
	tail := map[string]bool{got[2].ID: true, got[3].ID: true}
	if !tail["errored"] || !tail["empty"] {
		t.Errorf("failure tail missing entries: %+v", got[2:])
	}
}
