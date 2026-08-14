package distill

import "tenant/internal/memory/cosine"

// cosineSim computes cosine similarity between two embeddings. Delegates to
// the shared leaf package (internal/memory/cosine) — the single source of
// truth — kept as a thin alias so existing call sites read unchanged.
func cosineSim(a, b []float32) float64 {
	return cosine.Similarity(a, b)
}
