package main

import (
	"context"
	"fmt"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// runContextDebug (TEN-49) traces what the memory tiers would retrieve for a
// query — the same top-K semantic facts + episodes the assembler injects — so an
// operator can see WHY a query gets the context it does (or why it doesn't).
func runContextDebug(ctx context.Context, e *doctorEnv, query string) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("--context-debug needs a query, e.g. tenant doctor --context-debug \"what do I prefer\"")
	}
	if e.router == nil {
		return fmt.Errorf("no model router (run `tenant setup`); retrieval needs an embedder")
	}
	emb, _, err := e.router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil {
		return fmt.Errorf("resolve embedder: %w", err)
	}
	vecs, err := emb.Embed(ctx, []string{query})
	if err != nil || len(vecs) != 1 {
		return fmt.Errorf("embed query (is the embeddings endpoint up? run `tenant doctor`): %w", err)
	}
	qv := vecs[0]
	agent := e.c.agent

	fmt.Printf("context trace · agent=%q · query=%q · %dd embedding\n", agent, ctxClip(query, 80), len(qv))
	fmt.Println(strings.Repeat("─", 66))

	// --- Facts (T3 semantic): same top-K + agent/visibility filters the
	// assembler uses (retrieveFacts); deduped + budget-capped (~15% of the
	// window) at assemble time, not shown here. ---
	const factK = 12
	fmt.Printf("\n## Facts — top %d by relevance\n", factK)
	if e.semantic == nil {
		fmt.Println("  (facts.db not open)")
	} else if hits, ferr := e.semantic.Search(ctx, semantic.Query{
		AgentIDs:   ctxFilterAgent(agent),
		Visibility: []string{semantic.VisibilityPrivate, semantic.VisibilityShared},
		Embedding:  qv,
		Keywords:   query,
		K:          factK,
	}); ferr != nil {
		fmt.Printf("  search error: %v\n", ferr)
	} else if len(hits) == 0 {
		fmt.Println("  (none retrieved — empty store or nothing relevant)")
	} else {
		for i, h := range hits {
			fmt.Printf("  %2d. [score %.3f] %s\n", i+1, h.Score, ctxClip(ctxOneLine(h.Fact.Fact), 100))
		}
	}

	// --- Episodes (T2): recent conversation turns by relevance. ---
	const epK = 6
	fmt.Printf("\n## Episodes — top %d by relevance\n", epK)
	if e.episodic == nil {
		fmt.Println("  (episodes.db not open)")
	} else if hits, eerr := e.episodic.Search(ctx, episodic.Query{
		AgentIDs:  ctxFilterAgent(agent),
		Embedding: qv,
		Keywords:  query,
		K:         epK,
	}); eerr != nil {
		fmt.Printf("  search error: %v\n", eerr)
	} else if len(hits) == 0 {
		fmt.Println("  (none retrieved)")
	} else {
		for i, h := range hits {
			fmt.Printf("  %2d. [score %.3f] %s\n", i+1, h.Score, ctxClip(ctxOneLine(h.Episode.Prompt), 100))
		}
	}

	fmt.Println("\n" + strings.Repeat("─", 66))
	fmt.Println("This is the retrieval the agent would inject for that query (top-K; deduped + budget-capped at assemble time). Empty results usually mean the store is empty or the embeddings endpoint is down — run `tenant doctor`.")
	return nil
}

// ctxFilterAgent mirrors the assembler's filterAgent (retrieval scope: self,
// plus own sub-agents for an orchestrator).
func ctxFilterAgent(agentID string) []string {
	if agentID == "" {
		return nil
	}
	if strings.Contains(agentID, "-") {
		return []string{agentID}
	}
	return []string{agentID, agentID + "-*"}
}

func ctxOneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func ctxClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
