package semantic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Query is the fact search request. Same shape as episodic.Query plus
// an optional Now (for deterministic decay scoring in tests; zero means
// time.Now). Search filters out tombstoned + superseded facts; the audit
// path is Get + Count.
type Query struct {
	AgentIDs   []string
	Visibility []string
	Embedding  []float32
	Keywords   string // FTS5 MATCH expression
	K          int    // top-K, default 10
	After      time.Time
	Before     time.Time
	Now        time.Time // injection point for decay scoring; zero = real now
}

// Hit is a ranked search result. Score = RRF * effectiveConfidence,
// so facts that are both retrieved well AND fresh outrank stale-but-
// well-retrieved ones.
type Hit struct {
	Fact    *Fact
	Score   float64 // final rank score (rrf * decayed-confidence)
	VecRank int
	FTSRank int
}

// candidateLimit caps how many candidates each channel contributes to
// fusion. Larger = better recall at higher CPU cost; 50 suits
// personal-scale stores.
const candidateLimit = 50

// Search runs hybrid retrieval (vec + fts). The vector channel uses
// cosine SIMILARITY (magnitude, via fuseRelevance), not rank-only RRF —
// rank-only flattens on small candidate sets and let confidence
// reorder relevance (caught against real embeddings). The fused
// relevance is then modulated by each fact's time-decayed confidence.
// Fully-decayed facts (ec<=0) drop out — they naturally exit the ranking
// without explicit pruning.
func (s *Store) Search(ctx context.Context, q Query) ([]Hit, error) {
	if q.K <= 0 {
		q.K = 10
	}
	if len(q.Embedding) == 0 && q.Keywords == "" {
		return nil, fmt.Errorf("semantic: Search requires Embedding or Keywords")
	}
	now := q.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Vector channel carries the cosine SIMILARITY, not just rank.
	// RRF (rank-only) flattens to near-uniform scores on small
	// candidate sets — verified wrong against real embeddings, where
	// a 0.67-vs-0.51 cosine gap is far more informative than a
	// 1/61-vs-1/64 rank gap. Keep the magnitude.
	vecSim := map[int64]float64{}
	vecRank := map[int64]int{}
	if len(q.Embedding) > 0 {
		ranked, err := s.vectorSearch(ctx, q)
		if err != nil {
			return nil, err
		}
		for i, vh := range ranked {
			vecSim[vh.id] = vh.sim
			vecRank[vh.id] = i + 1
		}
	}

	ftsRank := map[int64]int{}
	if q.Keywords != "" {
		ranked, err := s.ftsSearch(ctx, q)
		if err != nil {
			return nil, err
		}
		for i, id := range ranked {
			ftsRank[id] = i + 1
		}
	}

	ids := map[int64]bool{}
	for id := range vecSim {
		ids[id] = true
	}
	for id := range ftsRank {
		ids[id] = true
	}

	// Load per-fact signals for the candidate set in one query. Missing
	// rows come back as the neutral default (importance=0.5, no heat), so
	// candidates with no signals row score EXACTLY as before this table
	// existed — the additive guarantee.
	idList := make([]int64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	sigs, err := s.SignalsBatch(ctx, idList)
	if err != nil {
		return nil, err
	}

	hits := make([]Hit, 0, len(ids))
	for id := range ids {
		fact, err := s.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("semantic: hydrate %d: %w", id, err)
		}
		sig, ok := sigs[id]
		if !ok {
			sig = defaultSignals(id)
		}
		// Importance stretches the decay horizon (load-bearing facts last
		// years); pinned never decays. Default importance ⇒ the old 365d curve.
		ec := fact.EffectiveConfidenceWithSignals(now, sig)
		// Decayed-to-zero facts drop out entirely (not a 0-score slot).
		if ec <= 0 {
			continue
		}
		relevance := fuseRelevance(vecSim[id], vecRank[id] > 0, ftsRank[id])
		// Quality MODULATES relevance within a bounded band — it can break
		// ties and gently favor trusted/important/hot facts, but can NEVER
		// reorder a clearly-more-relevant fact below a less-relevant one.
		// (Hard rrf*ec did exactly that; real embeddings caught it.)
		// Anchored so default signals reproduce the old 0.6+0.4*ec exactly.
		score := relevance * qualityModulator(ec, sig, now)
		hits = append(hits, Hit{
			Fact:    fact,
			Score:   score,
			VecRank: vecRank[id],
			FTSRank: ftsRank[id],
		})
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Fact.ID > hits[j].Fact.ID
	})
	if len(hits) > q.K {
		hits = hits[:q.K]
	}
	return hits, nil
}

// Quality-modulator weights. Importance and heat shift an effective
// "quality" term that is then clamped to [0,1] and fed through the SAME
// 0.6+0.4*q envelope the v1 code used for ec alone. Folding inside the
// envelope (rather than adding outside it) keeps the multiplier band at
// exactly [0.6, 1.0] — so the documented MODULATE-never-reorder invariant
// holds identically to before: importance/heat can lift a decayed-but-
// important fact up to (never past) the relevance-dominant ceiling.
const (
	importanceWeight = 0.4 // ±0.2 on the quality term across the importance range
	heatWeight       = 0.2 // up to +0.2 for a hot fact
)

// qualityModulator is the multiplier applied to relevance, in [0.6, 1.0].
// At the neutral default (importance=0.5, zero heat) it equals the legacy
// 0.6+0.4*ec exactly — the additive guarantee.
func qualityModulator(ec float64, sig Signals, now time.Time) float64 {
	q := ec
	q += importanceWeight * (sig.Importance - DefaultImportance)
	q += heatWeight * heatScore(sig, now)
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	return 0.6 + 0.4*q
}

// vecHit is one vector-search result carrying the cosine similarity
// (the magnitude the fusion needs, not just position).
type vecHit struct {
	id  int64
	sim float64
}

func (s *Store) vectorSearch(ctx context.Context, q Query) ([]vecHit, error) {
	where, args := buildFilterClause(q)
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.id, f.embedding FROM facts f WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic: vec scan: %w", err)
	}
	defer rows.Close()

	var all []vecHit
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("semantic: vec scan row: %w", err)
		}
		vec, err := decodeEmbedding(blob)
		if err != nil {
			continue
		}
		all = append(all, vecHit{id: id, sim: cosineSimilarity(q.Embedding, vec)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic: vec iter: %w", err)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].sim > all[j].sim })
	if len(all) > candidateLimit {
		all = all[:candidateLimit]
	}
	return all, nil
}

// fuseRelevance blends the vector (cosine, [~0,1]) and keyword (rank)
// channels into a single [0,1]-ish relevance. Cosine is the primary
// signal; FTS is a booster. Single-channel hits use that channel
// alone (keyword-only is down-weighted — a lexical match is weaker
// evidence than semantic similarity).
func fuseRelevance(cosine float64, hasVec bool, ftsRank int) float64 {
	v := cosine
	if v < 0 {
		v = 0 // negative cosine = unrelated; floor it
	}
	var fts float64
	if ftsRank > 0 {
		fts = 1.0 / float64(ftsRank) // rank1→1.0, rank2→0.5, rank3→0.33...
	}
	switch {
	case hasVec && ftsRank > 0:
		return 0.7*v + 0.3*fts
	case hasVec:
		return v
	default: // keyword-only
		return 0.5 * fts
	}
}

func (s *Store) ftsSearch(ctx context.Context, q Query) ([]int64, error) {
	where, args := buildFilterClause(q)
	args = append([]any{q.Keywords}, args...)
	rows, err := s.db.QueryContext(ctx, `
        SELECT f.id
        FROM facts_fts ff
        JOIN facts f ON f.id = ff.rowid
        WHERE ff.facts_fts MATCH ?
          AND `+where+`
        ORDER BY ff.rank
        LIMIT `+fmt.Sprint(candidateLimit),
		args...)
	if err != nil {
		return nil, fmt.Errorf("semantic: fts: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("semantic: fts scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic: fts iter: %w", err)
	}
	return out, nil
}

// buildFilterClause shares structure with the episodic equivalent but
// adds the "current fact" predicate: superseded_by IS NULL. Both tiers
// also exclude tombstoned. Aliased as "f" so vec and fts paths can
// share the snippet.
func buildFilterClause(q Query) (string, []any) {
	clauses := []string{"f.tombstoned = 0", "f.superseded_by IS NULL"}
	var args []any

	if len(q.AgentIDs) > 0 {
		clauses = append(clauses, "f.agent_id IN ("+placeholders(len(q.AgentIDs))+")")
		for _, id := range q.AgentIDs {
			args = append(args, id)
		}
	}
	if len(q.Visibility) > 0 {
		clauses = append(clauses, "f.visibility IN ("+placeholders(len(q.Visibility))+")")
		for _, v := range q.Visibility {
			args = append(args, v)
		}
	}
	if !q.After.IsZero() {
		clauses = append(clauses, "f.first_seen > ?")
		args = append(args, q.After.UTC().Unix())
	}
	if !q.Before.IsZero() {
		clauses = append(clauses, "f.first_seen < ?")
		args = append(args, q.Before.UTC().Unix())
	}
	return strings.Join(clauses, " AND "), args
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]string, n)
	for i := range out {
		out[i] = "?"
	}
	return strings.Join(out, ", ")
}
