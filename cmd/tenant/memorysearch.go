package main

// memorysearch.go (TEN-243 Phase B): the LOCAL host memory-recall tool. The TUI
// auto-assembles memory into context but never exposed an explicit recall tool;
// memory federation needs one to fold peers into (the symmetry of wiki_search).
//
// It searches THIS instance's own semantic (facts) + episodic (episodes) stores
// at FULL visibility — private included, because it's the user's own memory on
// their own machine. Registered DISABLED at launch and brought live when any
// peer is adopted (so memory_search then unifies local recall with peers' SHARED
// memory via the federation fan-out) or via a manual `/enable memory_search`.
// No peers + not manually enabled ⇒ the tool isn't in the catalog (memory stays
// auto-assembled exactly as before — strictly additive).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/ftsutil"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

type memorySearchDispatcher struct {
	selfName string
	semantic *semantic.Store
	episodic *episodic.Store
	embedder model.Embedder
}

func newMemorySearchDispatcher(name string, sem *semantic.Store, epi *episodic.Store, emb model.Embedder) *memorySearchDispatcher {
	if name == "" {
		name = "this tenant"
	}
	return &memorySearchDispatcher{selfName: name, semantic: sem, episodic: epi, embedder: emb}
}

func (d *memorySearchDispatcher) Tools() []model.ToolSpec {
	return []model.ToolSpec{{
		Name:        "memory_search",
		Description: "Recall from your own long-term memory — stored facts + past episodes — by semantic + keyword search. Covers your FULL memory (private included). Use when asked what you know/remember or about earlier conversations.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"k":{"type":"integer","description":"max results per store (default 8)"}},"required":["query"]}`),
	}}
}

func (d *memorySearchDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if call.Name != "memory_search" {
		return "unknown tool: " + call.Name, true, nil
	}
	var a peerSearchArgs
	if len(call.Arguments) > 0 {
		_ = json.Unmarshal(call.Arguments, &a)
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return "query is required", true, nil
	}
	k := clampK(a.K)

	// Embed for hybrid search if an embedder is available; degrade to
	// keyword-only on failure (never fatal — mirrors the peer tool).
	var embedding []float32
	if d.embedder != nil {
		if vecs, err := d.embedder.Embed(ctx, []string{q}); err == nil && len(vecs) == 1 {
			embedding = vecs[0]
		}
	}
	kw := ftsutil.Sanitize(q)

	var facts, episodes strings.Builder
	nFacts, nEps := 0, 0
	if d.semantic != nil {
		if hits, err := d.semantic.Search(ctx, semantic.Query{Embedding: embedding, Keywords: kw, K: k}); err == nil {
			nFacts = len(hits)
			for _, h := range hits {
				fmt.Fprintf(&facts, "- %s (%s, confidence %.2f)\n", capSnippet(h.Fact.Fact), h.Fact.Visibility, h.Fact.Confidence)
			}
		}
	}
	if d.episodic != nil {
		if hits, err := d.episodic.Search(ctx, episodic.Query{Embedding: embedding, Keywords: kw, K: k}); err == nil {
			nEps = len(hits)
			for _, h := range hits {
				e := h.Episode
				fmt.Fprintf(&episodes, "[%s, %s] %s -> %s\n", e.Timestamp.Format("2006-01-02"), e.Visibility, capSnippet(e.Prompt), capSnippet(e.Response))
			}
		}
	}
	if nFacts == 0 && nEps == 0 {
		return "(no results)", false, nil // clean empty signal — federation skips this cleanly
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Memory from %s\n", d.selfName)
	if nFacts > 0 {
		fmt.Fprintf(&b, "### Facts (%d)\n%s", nFacts, facts.String())
	}
	if nEps > 0 {
		fmt.Fprintf(&b, "### Episodes (%d)\n%s", nEps, episodes.String())
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}
