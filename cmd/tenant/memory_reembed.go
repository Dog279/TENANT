package main

// `tenant memory reembed` re-embeds every stored episode + fact with the
// CURRENT embedder, recovering retrieval after an embed-model switch (e.g. echo
// 128d → a real backend 768d, which the doctor flags as "embedding dimension
// consistency FAIL"). The source text is preserved; only the vectors + embedder
// id are rewritten. Idempotent: rows already at the live dimension are skipped.

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

func cmdMemoryReembed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("memory reembed", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	log := newLogger()
	router, err := buildRouter(c, log)
	if err != nil {
		return err
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		return err
	}
	defer closeStores()

	emb, _, err := router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil {
		return fmt.Errorf("resolve embedder: %w", err)
	}
	probe, err := emb.Embed(ctx, []string{"dim probe"})
	if err != nil || len(probe) != 1 {
		return fmt.Errorf("probe embedder dimension: %w", err)
	}
	liveDim := len(probe[0])
	embedderID := string(model.RoleEmbedder)
	fmt.Printf("re-embedding stored vectors to %dd (agent=%s, backend=%s)…\n", liveDim, c.agent, c.backend)

	eps, err := st.episodic.List(ctx, episodic.ListFilter{IncludeTombstoned: true})
	if err != nil {
		return fmt.Errorf("list episodes: %w", err)
	}
	epDone, epSkip, err := reembedEpisodes(ctx, emb, st.episodic, eps, liveDim, embedderID)
	if err != nil {
		return fmt.Errorf("reembed episodes: %w", err)
	}

	fcts, err := st.semantic.List(ctx, semantic.ListFilter{})
	if err != nil {
		return fmt.Errorf("list facts: %w", err)
	}
	fDone, fSkip, err := reembedFacts(ctx, emb, st.semantic, fcts, liveDim, embedderID)
	if err != nil {
		return fmt.Errorf("reembed facts: %w", err)
	}

	fmt.Printf("done: re-embedded %d episode(s) (%d already current) + %d fact(s) (%d already current).\n",
		epDone, epSkip, fDone, fSkip)
	fmt.Println("run `tenant doctor` to confirm the embedding-dimension check is green.")
	return nil
}

const reembedBatch = 16

// reembedEpisodes re-embeds each episode's PROMPT (the text the live recorder
// embeds — agent.go stores the query embedding), in batches.
func reembedEpisodes(ctx context.Context, emb model.Embedder, store *episodic.Store, eps []*episodic.Episode, liveDim int, embedderID string) (done, skipped int, err error) {
	type item struct {
		id   int64
		text string
	}
	var todo []item
	for _, e := range eps {
		if len(e.Embedding) == liveDim {
			skipped++
			continue
		}
		t := strings.TrimSpace(e.Prompt)
		if t == "" {
			t = strings.TrimSpace(e.Prompt + "\n" + e.Response)
		}
		if t == "" {
			continue // nothing to embed
		}
		todo = append(todo, item{e.ID, t})
	}
	for i := 0; i < len(todo); i += reembedBatch {
		end := min(i+reembedBatch, len(todo))
		texts := make([]string, 0, end-i)
		for _, it := range todo[i:end] {
			texts = append(texts, it.text)
		}
		vecs, eerr := emb.Embed(ctx, texts)
		if eerr != nil {
			return done, skipped, eerr
		}
		if len(vecs) != len(texts) {
			return done, skipped, fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(texts))
		}
		for j, it := range todo[i:end] {
			if uerr := store.UpdateEmbedding(ctx, it.id, vecs[j], embedderID); uerr != nil {
				return done, skipped, uerr
			}
			done++
		}
	}
	return done, skipped, nil
}

// reembedFacts re-embeds each fact's claim text, in batches.
func reembedFacts(ctx context.Context, emb model.Embedder, store *semantic.Store, fcts []*semantic.Fact, liveDim int, embedderID string) (done, skipped int, err error) {
	type item struct {
		id   int64
		text string
	}
	var todo []item
	for _, f := range fcts {
		if len(f.Embedding) == liveDim {
			skipped++
			continue
		}
		t := strings.TrimSpace(f.Fact)
		if t == "" {
			continue
		}
		todo = append(todo, item{f.ID, t})
	}
	for i := 0; i < len(todo); i += reembedBatch {
		end := min(i+reembedBatch, len(todo))
		texts := make([]string, 0, end-i)
		for _, it := range todo[i:end] {
			texts = append(texts, it.text)
		}
		vecs, eerr := emb.Embed(ctx, texts)
		if eerr != nil {
			return done, skipped, eerr
		}
		if len(vecs) != len(texts) {
			return done, skipped, fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(texts))
		}
		for j, it := range todo[i:end] {
			if uerr := store.UpdateEmbedding(ctx, it.id, vecs[j], embedderID); uerr != nil {
				return done, skipped, uerr
			}
			done++
		}
	}
	return done, skipped, nil
}
