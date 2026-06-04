package episodic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Query is the search request. Embedding and Keywords are independent
// signals — both, either, or neither can be set. K is the top-K cap
// applied after reciprocal-rank fusion. AgentIDs / Visibility / time
// bounds narrow the candidate set BEFORE scoring.
type Query struct {
	AgentIDs   []string  // OR semantics; empty = all
	Visibility []string  // OR semantics; empty = all (private | shared | public)
	Embedding  []float32 // vector for cosine similarity (optional)
	Keywords   string    // FTS5 MATCH expression (optional, e.g. "go AND agent")
	K          int       // top-K after fusion; defaults to 10
	After      time.Time // ts > After (zero = no lower bound)
	Before     time.Time // ts < Before (zero = no upper bound)
}

// Hit is one ranked search result. Score is the RRF-fused score (higher
// = better). VecRank and FTSRank are the per-signal ranks (1-indexed,
// 0 = not in that result list).
type Hit struct {
	Episode *Episode
	Score   float64
	VecRank int
	FTSRank int
}

// candidateLimit caps how many candidates each channel contributes to
// fusion. Larger = better recall at higher CPU cost; 50 suits
// personal-scale stores. (Fusion is cosine-magnitude based — see
// fuseRelevance — not rank-only RRF.)
const candidateLimit = 50

// Search runs the hybrid retrieval pipeline:
//
//  1. Pull filtered candidates (agent + visibility + time + non-tombstoned).
//  2. If Query.Embedding set: cosine score every candidate, take top-N by score.
//  3. If Query.Keywords set: run FTS5 MATCH against the same filter, take top-N by rank.
//  4. Reciprocal-rank-fuse the two lists into one combined ranking.
//  5. Hydrate the top-K hits with full Episode data.
//
// Brute-force cosine is fine up to ~100K filtered candidates. Past
// that, swap to sqlite-vec (CGO) or pgvector. The Query / Hit API
// stays the same across that swap.
func (s *Store) Search(ctx context.Context, q Query) ([]Hit, error) {
	if q.K <= 0 {
		q.K = 10
	}
	if len(q.Embedding) == 0 && q.Keywords == "" {
		return nil, fmt.Errorf("episodic: Search requires Embedding or Keywords")
	}

	// Vector channel carries cosine SIMILARITY (magnitude), not just
	// rank — rank-only RRF is near-flat on small candidate sets.
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

	hits := make([]Hit, 0, len(ids))
	for id := range ids {
		ep, err := s.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("episodic: hydrate %d: %w", id, err)
		}
		// Episodes have no confidence — relevance IS the score.
		hits = append(hits, Hit{
			Episode: ep,
			Score:   fuseRelevance(vecSim[id], vecRank[id] > 0, ftsRank[id]),
			VecRank: vecRank[id],
			FTSRank: ftsRank[id],
		})
	}

	// Sort descending by score. Tiebreak by id (newer rows = higher
	// ids — recency as a soft second key).
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Episode.ID > hits[j].Episode.ID
	})
	if len(hits) > q.K {
		hits = hits[:q.K]
	}
	return hits, nil
}

// fuseRelevance blends the vector (cosine, [~0,1]) and keyword (rank)
// channels into one [0,1]-ish relevance. Cosine is the primary signal;
// FTS is a booster. Keyword-only hits are down-weighted (a lexical
// match is weaker evidence than semantic similarity). Identical to the
// semantic tier's fusion — kept per-package to avoid a cross-import.
func fuseRelevance(cosine float64, hasVec bool, ftsRank int) float64 {
	v := cosine
	if v < 0 {
		v = 0
	}
	var fts float64
	if ftsRank > 0 {
		fts = 1.0 / float64(ftsRank)
	}
	switch {
	case hasVec && ftsRank > 0:
		return 0.7*v + 0.3*fts
	case hasVec:
		return v
	default:
		return 0.5 * fts
	}
}

// vectorSearch loads all candidates matching the filter, scores by
// cosine similarity, returns IDs ordered by descending similarity.
// Capped at candidateLimit.
// vecHit carries the cosine similarity (magnitude the fusion needs).
type vecHit struct {
	id  int64
	sim float64
}

func (s *Store) vectorSearch(ctx context.Context, q Query) ([]vecHit, error) {
	where, args := buildFilterClause(q)
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, e.embedding FROM episodes e WHERE `+where, args...)
	if err != nil {
		// Graceful degradation on disk-level corruption: empty result + log,
		// don't kill the whole assemble step. Recovery is operator action
		// (rm episodes.db / wal / shm) — but until they do, the agent can
		// still run with a degraded memory tier instead of failing every
		// /goal and /research call. Live trigger: user's episodes.db got
		// corrupted (WAL not checkpointed before a kill); every subsequent
		// call to /goal --agent main died at "vec scan: database disk
		// image is malformed (11)".
		if isCorruptionError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("episodic: vec scan: %w", err)
	}
	defer rows.Close()

	var all []vecHit
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			if isCorruptionError(err) {
				return all, nil // return what we have, drop the rest silently
			}
			return nil, fmt.Errorf("episodic: vec scan row: %w", err)
		}
		vec, err := decodeEmbedding(blob)
		if err != nil {
			continue // skip malformed rows rather than killing the search
		}
		all = append(all, vecHit{id: id, sim: cosineSimilarity(q.Embedding, vec)})
	}
	if err := rows.Err(); err != nil {
		if isCorruptionError(err) {
			return all, nil
		}
		return nil, fmt.Errorf("episodic: vec iter: %w", err)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].sim > all[j].sim })
	if len(all) > candidateLimit {
		all = all[:candidateLimit]
	}
	return all, nil
}

// ftsSearch runs the FTS5 MATCH query and returns IDs by rank.
// Joins back to episodes to apply the agent/visibility/tombstone/time
// filter that FTS5 doesn't know about.
func (s *Store) ftsSearch(ctx context.Context, q Query) ([]int64, error) {
	where, args := buildFilterClause(q)
	// FTS5's bm25 rank: smaller is better. We invert via -bm25 to use
	// "ORDER BY rank DESC" semantics consistently with the vec path.
	args = append([]any{q.Keywords}, args...)
	rows, err := s.db.QueryContext(ctx, `
        SELECT e.id
        FROM episodes_fts f
        JOIN episodes e ON e.id = f.rowid
        WHERE f.episodes_fts MATCH ?
          AND `+where+`
        ORDER BY f.rank
        LIMIT `+fmt.Sprint(candidateLimit),
		args...)
	if err != nil {
		// Same graceful-degradation policy as vectorSearch — corrupt DB
		// shouldn't kill /goal or /research, just degrade FTS to empty.
		if isCorruptionError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("episodic: fts: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			if isCorruptionError(err) {
				return out, nil
			}
			return nil, fmt.Errorf("episodic: fts scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		if isCorruptionError(err) {
			return out, nil
		}
		return nil, fmt.Errorf("episodic: fts iter: %w", err)
	}
	return out, nil
}

// buildFilterClause renders the WHERE fragment shared by vec + fts
// paths. Returns the SQL snippet (without the leading "WHERE") and
// the positional args. Uses "e." prefix in the fts path; works
// unprefixed in the vec path because the vec query selects from
// episodes directly. Caller wraps as appropriate.
func buildFilterClause(q Query) (string, []any) {
	var clauses []string
	var args []any

	// FTS path joins via "e" alias; vec path queries the table directly.
	// Use "e." prefix in both — the vec path's SELECT FROM episodes
	// allows column qualification with the table name.
	clauses = append(clauses, "e.tombstoned = 0")

	if len(q.AgentIDs) > 0 {
		// Two sub-clauses: exact-match IDs (IN) and glob IDs ending in `*`
		// (LIKE). The orchestrator-vs-sub-agent distinction in
		// assemble.filterAgent expands "main" → ["main", "main-*"]; this
		// builder turns the suffix-`*` form into SQL LIKE (TEN-45).
		var exactIDs []string
		var likes []string
		for _, id := range q.AgentIDs {
			if strings.HasSuffix(id, "*") {
				likes = append(likes, "e.agent_id LIKE ?")
				args = append(args, strings.TrimSuffix(id, "*")+"%")
			} else {
				exactIDs = append(exactIDs, id)
			}
		}
		var parts []string
		if len(exactIDs) > 0 {
			parts = append(parts, "e.agent_id IN ("+placeholders(len(exactIDs))+")")
			for _, id := range exactIDs {
				args = append(args, id)
			}
		}
		parts = append(parts, likes...)
		// Re-order args: LIKE args were appended above but the IN args
		// come first in the rendered SQL. Reorder the args slice so
		// positional placeholders line up. The dance is necessary
		// because we appended in mixed order during the loop.
		// Easier: rebuild args in the correct order.
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
		// Reorder args to match: IN-args first, then one LIKE-arg per LIKE clause.
		// We have to recompute because the loop above interleaved them.
		// (Done below — replace the loop args with a clean rebuild.)
		args = args[:len(args)-len(q.AgentIDs)] // strip what we just appended
		for _, id := range exactIDs {
			args = append(args, id)
		}
		for _, id := range q.AgentIDs {
			if strings.HasSuffix(id, "*") {
				args = append(args, strings.TrimSuffix(id, "*")+"%")
			}
		}
	}
	if len(q.Visibility) > 0 {
		clauses = append(clauses, "e.visibility IN ("+placeholders(len(q.Visibility))+")")
		for _, v := range q.Visibility {
			args = append(args, v)
		}
	}
	if !q.After.IsZero() {
		clauses = append(clauses, "e.ts > ?")
		args = append(args, q.After.UTC().Unix())
	}
	if !q.Before.IsZero() {
		clauses = append(clauses, "e.ts < ?")
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

// isCorruptionError matches the SQLite "database disk image is malformed"
// and related corruption errors (SQLITE_CORRUPT / SQLITE_NOTADB). When this
// hits a read path, we degrade to empty results rather than killing the
// whole assemble — the operator must recover the file out-of-band, but
// /goal, /research, and normal chat can still run with a degraded memory
// tier. Better than blocking everything until they fix the disk.
func isCorruptionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"database disk image is malformed",
		"file is not a database",
		"SQLITE_CORRUPT",
		"SQLITE_NOTADB",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
