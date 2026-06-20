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
	"path/filepath"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
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

	// Skills (T4): re-embed wrong-dimension rows AND backfill the float32 BLOB
	// for legacy JSON-only rows (TEN-283). The skills store isn't part of the
	// reembed `stores` bundle, so open it directly against the same data dir.
	sk, serr := skills.Open(filepath.Join(c.dataDir, "skills.db"))
	if serr != nil {
		return fmt.Errorf("open skills: %w", serr)
	}
	defer sk.Close()
	sDone, sSkip, err := reembedSkills(ctx, emb, sk, liveDim, embedderID)
	if err != nil {
		return fmt.Errorf("reembed skills: %w", err)
	}

	fmt.Printf("done: re-embedded %d episode(s) (%d already current) + %d fact(s) (%d already current) + %d skill(s) (%d already current).\n",
		epDone, epSkip, fDone, fSkip, sDone, sSkip)
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

// reembedSkills re-embeds each skill's "Name: Description" text (the same text
// the live AddSkill path embeds) and, crucially, backfills the float32 BLOB
// (TEN-283) for any legacy JSON-only row — even when its existing vector is
// already at the live dimension. UpdateEmbedding writes BOTH the BLOB and the
// JSON, so one pass migrates the storage format AND fixes any dimension drift.
func reembedSkills(ctx context.Context, emb model.Embedder, store *skills.Store, liveDim int, embedderID string) (done, skipped int, err error) {
	all, lerr := store.ListAll(ctx)
	if lerr != nil {
		return 0, 0, lerr
	}
	// Rows still in JSON-only form must be re-embedded regardless of dimension,
	// so the BLOB gets written. Same-dim rows that already HAVE a BLOB are skipped.
	blobless, berr := store.BloblessIDs(ctx)
	if berr != nil {
		return 0, 0, berr
	}
	needsBlob := make(map[int64]bool, len(blobless))
	for _, id := range blobless {
		needsBlob[id] = true
	}

	type item struct {
		id   int64
		text string
	}
	var todo []item
	for _, s := range all {
		if len(s.Embedding) == liveDim && !needsBlob[s.ID] {
			skipped++
			continue
		}
		t := strings.TrimSpace(s.Name + ": " + s.Description)
		if strings.TrimSpace(s.Description) == "" {
			continue // nothing meaningful to embed
		}
		todo = append(todo, item{s.ID, t})
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
