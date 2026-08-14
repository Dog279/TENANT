package main

// `tenant memory import <file.md>` ports operator-authored markdown notes
// (Hermes-style MEMORY.md / USER.md / FOUNDER_PROFILE.md — non-derivable
// context the model can't reconstruct from episodes) into the T3 semantic
// store as durable facts (TEN-279).
//
// Each meaningful bullet / prose line becomes one atomic claim (see
// internal/memory/mdimport): it is embedded with the live embedder — EXACTLY
// the resolve/openStores/EmbedderForRole path `memory reembed` uses — and
// Inserted as a fact for the agent. importance (--importance) and the
// merge-protect flag (--protected, for feedback/correction files) are written
// to the fact_signals side table. The import is idempotent: a claim whose
// normalized text already exists as a live fact for the agent is skipped, so
// re-running never duplicates. --dry-run reports what WOULD be imported.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"tenant/internal/memory/mdimport"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

const importBatch = 16

// defaultImportImportance sits above neutral (0.5): operator-curated
// non-derivable notes are worth more than an auto-distilled fact and should
// out-live the default decay horizon, but stay below ProtectImportance (0.9)
// so they don't auto-starve consolidation unless --protected is set.
const defaultImportImportance = 0.7

func cmdMemoryImport(ctx context.Context, args []string) error {
	// Go's flag package stops at the first non-flag arg, so a natural
	// `memory import path/to/file.md --backend echo` would leave the flags
	// unparsed. Split the leading non-flag token (the file path) from the
	// flag tail — same trick as `memory search` (commands.go).
	split := len(args)
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			split = i
			break
		}
	}
	leading := args[:split]
	if len(leading) != 1 {
		return fmt.Errorf("usage: tenant memory import <file.md> [--protected] [--importance N] [--dry-run]")
	}
	path := leading[0]

	fs := flag.NewFlagSet("memory import", flag.ContinueOnError)
	c := bindCommon(fs)
	protected := fs.Bool("protected", false, "mark imported facts as merge-protected (use for feedback/correction notes)")
	importance := fs.Float64("importance", defaultImportImportance, "importance signal for imported facts, 0..1 (default 0.7; >neutral so operator notes out-live decay)")
	dryRun := fs.Bool("dry-run", false, "print what would be imported without writing")
	if err := fs.Parse(args[split:]); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}

	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	claims := mdimport.Parse(string(src))
	if len(claims) == 0 {
		fmt.Printf("%s: 0 claims parsed (nothing to import)\n", path)
		return nil
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
	embedderID := string(model.RoleEmbedder)

	res, err := importClaims(ctx, emb, st.semantic, c.agent, claims, importParams{
		EmbedderID: embedderID,
		Importance: *importance,
		Protected:  *protected,
		DryRun:     *dryRun,
	})
	if err != nil {
		return err
	}

	mode := ""
	if *dryRun {
		mode = " (dry-run — nothing written)"
	}
	fmt.Printf("%s%s\n", path, mode)
	fmt.Printf("  parsed:   %d\n", res.Parsed)
	if *dryRun {
		fmt.Printf("  would import: %d  (already present: %d)\n", res.WouldImport, res.SkippedDup)
		for _, line := range res.Preview {
			fmt.Printf("    + %s\n", line)
		}
		if res.WouldImport > len(res.Preview) {
			fmt.Printf("    … and %d more\n", res.WouldImport-len(res.Preview))
		}
		return nil
	}
	fmt.Printf("  inserted: %d\n", res.Inserted)
	fmt.Printf("  skipped (duplicate): %d\n", res.SkippedDup)
	prot := ""
	if *protected {
		prot = ", protected"
	}
	fmt.Printf("  signals:  importance=%.2f%s (agent=%s)\n", *importance, prot, c.agent)
	return nil
}

type importParams struct {
	EmbedderID string
	Importance float64
	Protected  bool
	DryRun     bool
}

type importResult struct {
	Parsed      int
	Inserted    int
	SkippedDup  int
	WouldImport int
	Preview     []string // first few claim texts that would be imported (dry-run)
}

const previewLines = 8

// importClaims embeds and inserts each parsed claim as a T3 fact for agentID,
// skipping any whose normalized text already exists as a live fact (idempotent
// re-import). It mirrors reembedFacts' batched-embed shape. When p.DryRun is
// set it computes the same plan but writes nothing. Importance/Protected are
// applied via the fact_signals side table after a successful Insert.
func importClaims(ctx context.Context, emb model.Embedder, store *semantic.Store, agentID string, claims []mdimport.Claim, p importParams) (importResult, error) {
	res := importResult{Parsed: len(claims)}

	// Build the existing-fact key set ONCE: normalized text of every live
	// fact already stored for this agent. Re-import then skips anything that
	// already landed (including across multiple source files).
	existing, err := store.List(ctx, semantic.ListFilter{AgentIDs: []string{agentID}})
	if err != nil {
		return res, fmt.Errorf("list existing facts: %w", err)
	}
	seen := make(map[string]struct{}, len(existing))
	for _, f := range existing {
		seen[mdimport.Normalize(f.Fact)] = struct{}{}
	}

	// Filter to the claims that are genuinely new (and not dup'd within this
	// run — Parse already de-dups, but guard against collisions with existing).
	var todo []mdimport.Claim
	for _, cl := range claims {
		if _, dup := seen[cl.Norm]; dup {
			res.SkippedDup++
			continue
		}
		seen[cl.Norm] = struct{}{} // guard within this run too
		todo = append(todo, cl)
	}

	if p.DryRun {
		res.WouldImport = len(todo)
		for i := 0; i < len(todo) && i < previewLines; i++ {
			res.Preview = append(res.Preview, clip(todo[i].Text, 90))
		}
		return res, nil
	}

	for i := 0; i < len(todo); i += importBatch {
		end := min(i+importBatch, len(todo))
		texts := make([]string, 0, end-i)
		for _, cl := range todo[i:end] {
			texts = append(texts, cl.Text)
		}
		vecs, eerr := emb.Embed(ctx, texts)
		if eerr != nil {
			return res, fmt.Errorf("embed claims: %w", eerr)
		}
		if len(vecs) != len(texts) {
			return res, fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(texts))
		}
		for j, cl := range todo[i:end] {
			id, ierr := store.Insert(ctx, &semantic.Fact{
				AgentID:    agentID,
				Fact:       cl.Text,
				Confidence: 1.0,
				EmbedderID: p.EmbedderID,
				Embedding:  vecs[j],
			})
			if ierr != nil {
				return res, fmt.Errorf("insert fact: %w", ierr)
			}
			// Write importance / protected to the side table. A non-default
			// importance or the protect flag both warrant a signals row;
			// when neither differs from the neutral default we still write
			// importance so the operator's intent is explicit and durable.
			if serr := store.UpsertSignals(ctx, semantic.Signals{
				FactID:     id,
				Importance: p.Importance,
				Protected:  p.Protected,
			}); serr != nil {
				return res, fmt.Errorf("set signals for fact %d: %w", id, serr)
			}
			res.Inserted++
		}
	}
	return res, nil
}
