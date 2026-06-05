// Package distill implements the periodic job that turns T2 episodes
// into T3 facts. It is the "agent learns over time" loop: read recent
// turn-pairs, ask the summarizer LLM to extract atomic claims about
// the user / project / preferences, and write those claims as facts
// into the semantic store.
//
// Design notes:
//
//   - One-way data flow: episodic → semantic. Distillation never
//     mutates episodes.
//
//   - Cursor-based: caller passes the last-processed episode ID,
//     gets back the new cursor. State persistence is the caller's
//     job (a meta table, a state file, or memory if the agent runs
//     long enough to keep the cursor in process).
//
//   - Re-affirmation, not deduplication. When a new fact's embedding
//     is very similar to an existing fact's, we Reaffirm instead of
//     Insert — the old fact's last_confirmed gets bumped, decay
//     resets. This is how facts stay fresh over a long-running agent.
//
//   - Supersede is NOT implemented in v1. Detecting that a new fact
//     contradicts (vs. merely covers similar territory) needs LLM
//     reasoning between facts, which is a separate call cycle.
//     Captured as a TODO. Until then, contradictions accumulate; the
//     decay function handles staleness organically.
package distill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// Defaults — exposed as exported vars so an operator can tune at
// startup without forking the package.
var (
	DefaultBatchSize = 15
	// DefaultSimilarityThreshold: cosine >= this auto-reaffirms (treats the
	// new extraction as the same fact). Lowered from 0.92 so closer
	// paraphrases dedup instead of inserting near-duplicates.
	DefaultSimilarityThreshold = 0.88
	// DefaultBorderlineThreshold: cosine in [borderline, similarity) is
	// ambiguous — the summarizer adjudicates restate-vs-distinct instead of
	// blindly inserting a possible paraphrase.
	DefaultBorderlineThreshold = 0.80
	DefaultSummarizerRole      = model.RoleSummarizer
	DefaultEmbedderRole        = model.RoleEmbedder
)

// Distiller is the entry point. Construct once, call Run as often as
// your cadence policy dictates.
type Distiller struct {
	Router              *model.Router
	Episodic            *episodic.Store
	Semantic            *semantic.Store
	AgentID             string
	BatchSize           int
	SimilarityThreshold float64
	SummarizerRole      model.Role
	EmbedderRole        model.Role
	Logger              *slog.Logger
}

// RunResult summarizes what one Distill.Run did. The caller persists
// LastEpisodeID and passes it as `since` to the next run.
type RunResult struct {
	EpisodesProcessed int
	BatchesAttempted  int
	FactsExtracted    int
	FactsInserted     int
	FactsReaffirmed   int
	LastEpisodeID     int64
	// BatchErrors holds non-fatal errors from individual batches
	// (e.g. LLM returned malformed JSON for batch 3). The run
	// continues past these; the caller decides whether to alert.
	BatchErrors []error
}

// Run scans episodes with id > sinceEpisodeID for the configured
// AgentID, batches them, and asks the summarizer LLM to extract
// facts. New facts (or sufficiently distinct restatements) land in
// the semantic store; close-enough matches Reaffirm existing facts.
//
// Returns immediately with EpisodesProcessed=0 if there's nothing
// new. Safe to call concurrently with the agent loop — the episodic
// + semantic stores handle their own concurrency.
func (d *Distiller) Run(ctx context.Context, sinceEpisodeID int64) (*RunResult, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	batchSize := d.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	threshold := d.SimilarityThreshold
	if threshold <= 0 {
		threshold = DefaultSimilarityThreshold
	}

	// 1. Fetch all unprocessed episodes for this agent.
	episodes, err := d.Episodic.List(ctx, episodic.ListFilter{
		AgentIDs: []string{d.AgentID},
		SinceID:  sinceEpisodeID,
	})
	if err != nil {
		return nil, fmt.Errorf("distill: list episodes: %w", err)
	}
	result := &RunResult{LastEpisodeID: sinceEpisodeID}
	if len(episodes) == 0 {
		return result, nil
	}

	// 2. Resolve the two LLMs we need: summarizer (text generation)
	//    and embedder (for similarity-driven Reaffirm vs Insert).
	summarizer, _, err := d.Router.LLMForRole(ctx, d.summarizerRole())
	if err != nil {
		return nil, fmt.Errorf("distill: resolve summarizer: %w", err)
	}
	embedder, embProfile, err := d.Router.EmbedderForRole(ctx, d.embedderRole())
	if err != nil {
		return nil, fmt.Errorf("distill: resolve embedder: %w", err)
	}

	// 3. Process in batches.
	for start := 0; start < len(episodes); start += batchSize {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		end := start + batchSize
		if end > len(episodes) {
			end = len(episodes)
		}
		batch := episodes[start:end]
		result.BatchesAttempted++

		extracted, err := d.extractBatch(ctx, summarizer, batch)
		if err != nil {
			// Non-fatal: log + continue. One bad LLM batch shouldn't
			// strand the cursor; the next run will scan past these.
			log.Warn("distill: batch extract failed", "agent", d.AgentID, "batch_start", start, "err", err)
			result.BatchErrors = append(result.BatchErrors, fmt.Errorf("batch starting at episode %d: %w", batch[0].ID, err))
			result.EpisodesProcessed += len(batch)
			result.LastEpisodeID = batch[len(batch)-1].ID
			continue
		}

		result.FactsExtracted += len(extracted)

		// 4. Embed all extracted facts in one call (cheaper than per-fact).
		ins, reaff, err := d.persistFacts(ctx, summarizer, embedder, string(embProfile.ID), extracted, threshold)
		if err != nil {
			log.Warn("distill: persist failed", "agent", d.AgentID, "batch_start", start, "err", err)
			result.BatchErrors = append(result.BatchErrors, fmt.Errorf("persist for batch starting at episode %d: %w", batch[0].ID, err))
		}
		result.FactsInserted += ins
		result.FactsReaffirmed += reaff
		result.EpisodesProcessed += len(batch)
		result.LastEpisodeID = batch[len(batch)-1].ID
	}

	return result, nil
}

func (d *Distiller) validate() error {
	if d.Router == nil {
		return errors.New("distill: Router required")
	}
	if d.Episodic == nil {
		return errors.New("distill: Episodic store required")
	}
	if d.Semantic == nil {
		return errors.New("distill: Semantic store required")
	}
	if d.AgentID == "" {
		return errors.New("distill: AgentID required")
	}
	return nil
}

func (d *Distiller) summarizerRole() model.Role {
	if d.SummarizerRole != "" {
		return d.SummarizerRole
	}
	return DefaultSummarizerRole
}

func (d *Distiller) embedderRole() model.Role {
	if d.EmbedderRole != "" {
		return d.EmbedderRole
	}
	return DefaultEmbedderRole
}

// --- batch extraction ---

// extractedFact is the wire shape we expect back from the summarizer.
type extractedFact struct {
	Fact             string  `json:"fact"`
	Confidence       float64 `json:"confidence"`
	SourceEpisodeIDs []int64 `json:"source_episode_ids"`
}

type extractedBatch struct {
	Facts []extractedFact `json:"facts"`
}

// extractBatch sends one batch of episodes to the summarizer and
// returns the extracted facts. Each fact lists which episode IDs in
// the batch supported it.
func (d *Distiller) extractBatch(ctx context.Context, llm model.LLM, batch []*episodic.Episode) ([]extractedFact, error) {
	prompt := buildPrompt(batch)
	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
		JSONSchema:  []byte(factsJSONSchema),
		Temperature: 0.1, // low: we want consistent extraction, not creative writing
	})
	if err != nil {
		return nil, fmt.Errorf("summarizer: %w", err)
	}
	if resp.Text == "" {
		return nil, errors.New("summarizer returned empty response")
	}
	// Real models (Gemma, Llama, Qwen) frequently wrap JSON in
	// ```json fences or add prose around it, and vLLM guided_json is
	// not reliably enforced across builds. Never trust grammar
	// constraints — defensively extract the JSON object before parsing.
	jsonText := extractJSONObject(resp.Text)
	var out extractedBatch
	if err := json.Unmarshal([]byte(jsonText), &out); err != nil {
		return nil, fmt.Errorf("parse summarizer JSON: %w (response: %q)", err, snippet(resp.Text, 200))
	}
	// Filter obvious garbage: empty fact text, out-of-batch source IDs.
	clean := make([]extractedFact, 0, len(out.Facts))
	batchIDs := make(map[int64]bool, len(batch))
	for _, e := range batch {
		batchIDs[e.ID] = true
	}
	for _, f := range out.Facts {
		if f.Fact == "" {
			continue
		}
		if f.Confidence <= 0 {
			f.Confidence = 0.7 // sensible default for unrated extraction
		}
		if f.Confidence > 1 {
			f.Confidence = 1
		}
		valid := make([]int64, 0, len(f.SourceEpisodeIDs))
		for _, id := range f.SourceEpisodeIDs {
			if batchIDs[id] {
				valid = append(valid, id)
			}
		}
		f.SourceEpisodeIDs = valid
		clean = append(clean, f)
	}
	return clean, nil
}

// persistFacts embeds each extracted fact, finds its closest existing fact,
// and either reaffirms (clear match), reaffirms after summarizer adjudication
// (borderline match), or inserts (distinct). Returns (inserted, reaffirmed,
// error).
func (d *Distiller) persistFacts(ctx context.Context, summarizer model.LLM, embedder model.Embedder, embedderID string, facts []extractedFact, threshold float64) (int, int, error) {
	if len(facts) == 0 {
		return 0, 0, nil
	}
	texts := make([]string, len(facts))
	for i, f := range facts {
		texts[i] = f.Fact
	}
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return 0, 0, fmt.Errorf("embed facts: %w", err)
	}
	if len(vectors) != len(facts) {
		return 0, 0, fmt.Errorf("embedder returned %d vectors for %d facts", len(vectors), len(facts))
	}

	var inserted, reaffirmed int
	for i, f := range facts {
		if err := ctx.Err(); err != nil {
			return inserted, reaffirmed, err
		}
		// Look up the closest existing fact for this agent.
		existing, err := d.findClosest(ctx, vectors[i])
		if err != nil {
			return inserted, reaffirmed, err
		}
		// Clear match → reaffirm the existing fact.
		if existing != nil && existing.Score >= threshold {
			if err := d.Semantic.Reaffirm(ctx, existing.Fact.ID); err != nil {
				return inserted, reaffirmed, fmt.Errorf("reaffirm fact %d: %w", existing.Fact.ID, err)
			}
			reaffirmed++
			continue
		}
		// Borderline match → let the summarizer decide restate vs. distinct,
		// so a reworded duplicate reaffirms instead of inserting a near-dup.
		if existing != nil && existing.Score >= DefaultBorderlineThreshold && summarizer != nil {
			same, aerr := d.isRestatement(ctx, summarizer, f.Fact, existing.Fact.Fact)
			if aerr != nil {
				log := d.Logger
				if log == nil {
					log = slog.Default()
				}
				log.Warn("distill: borderline adjudication failed; inserting", "err", aerr)
			} else if same {
				if err := d.Semantic.Reaffirm(ctx, existing.Fact.ID); err != nil {
					return inserted, reaffirmed, fmt.Errorf("reaffirm fact %d: %w", existing.Fact.ID, err)
				}
				reaffirmed++
				continue
			}
		}
		_, err = d.Semantic.Insert(ctx, &semantic.Fact{
			AgentID:        d.AgentID,
			Visibility:     semantic.VisibilityPrivate,
			Fact:           f.Fact,
			SourceEpisodes: f.SourceEpisodeIDs,
			Confidence:     f.Confidence,
			EmbedderID:     embedderID,
			Embedding:      vectors[i],
		})
		if err != nil {
			return inserted, reaffirmed, fmt.Errorf("insert fact: %w", err)
		}
		inserted++
	}
	return inserted, reaffirmed, nil
}

const restatementSystemPrompt = `You decide whether two memory facts express the SAME underlying claim about a user/project (one may be a reworded, or less/more specific, version of the other) or are DISTINCT claims.

Respond with JSON only: {"same": true} if they are the same underlying fact, {"same": false} if distinct.`

const restatementJSONSchema = `{"type":"object","properties":{"same":{"type":"boolean"}},"required":["same"]}`

// isRestatement asks the summarizer whether `candidate` restates `existing`
// (same underlying claim) vs. a distinct fact. Used only for borderline-
// similar pairs to avoid inserting paraphrase duplicates.
func (d *Distiller) isRestatement(ctx context.Context, llm model.LLM, candidate, existing string) (bool, error) {
	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: restatementSystemPrompt},
			{Role: "user", Content: fmt.Sprintf("EXISTING: %s\nNEW: %s\n\nIs NEW the same underlying fact as EXISTING?", existing, candidate)},
		},
		JSONSchema:  []byte(restatementJSONSchema),
		Temperature: 0,
	})
	if err != nil {
		return false, fmt.Errorf("summarizer: %w", err)
	}
	if resp.Text == "" {
		return false, errors.New("summarizer returned empty")
	}
	var out struct {
		Same bool `json:"same"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(resp.Text)), &out); err != nil {
		return false, fmt.Errorf("parse adjudication: %w (%q)", err, snippet(resp.Text, 120))
	}
	return out.Same, nil
}

// closestHit wraps a semantic.Hit with the raw cosine score reported
// by Search. semantic.Hit.Score is the RRF * decay product; we want
// the per-tier cosine for similarity-vs-rephrase decisions, which we
// approximate here by extracting the top hit and reading its rank.
//
// For v1 we use semantic.Search's K=1 result and trust that a vec
// rank of 1 with the same embedder means "close enough" — augmented
// by RRF's score curve. A future v1.1 will plumb raw cosine through.
type closestHit struct {
	Fact  *semantic.Fact
	Score float64
}

func (d *Distiller) findClosest(ctx context.Context, embedding []float32) (*closestHit, error) {
	hits, err := d.Semantic.Search(ctx, semantic.Query{
		AgentIDs:  []string{d.AgentID},
		Embedding: embedding,
		K:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("search semantic: %w", err)
	}
	if len(hits) == 0 {
		return nil, nil
	}
	// Approximate "is this the same fact?" via direct cosine between
	// the new embedding and the existing fact's embedding. Search
	// returned the top candidate; we just verify similarity here.
	sim := cosineSim(embedding, hits[0].Fact.Embedding)
	return &closestHit{Fact: hits[0].Fact, Score: sim}, nil
}

// snippet for log messages.
func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// extractJSONObject pulls the first balanced top-level JSON object out
// of noisy LLM output: ```json fences, leading prose ("Here are the
// facts:"), trailing commentary, etc. Strategy: find the first '{',
// then scan for its matching '}' respecting string literals + escapes.
// Falls back to the trimmed input if no object is found (json.Unmarshal
// then produces the real error for diagnostics).
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return strings.TrimSpace(s)
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	// Unbalanced (model got truncated mid-object) — return from the
	// first brace so the parser error points at the right place.
	return s[start:]
}
