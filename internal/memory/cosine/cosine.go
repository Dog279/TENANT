// Package cosine is the single source of truth for cosine similarity over
// float32 embedding vectors. It deliberately imports NOTHING from
// internal/memory/* (or anywhere else in the tree beyond the standard
// library) so every memory tier — episodic, semantic, distill, skills — and
// every plugin/command can import it WITHOUT risking an import cycle. This
// replaces the ~9 byte-for-byte copies of the same formula that had been
// inlined across the tree to dodge that cycle.
package cosine

import "math"

// Similarity returns the cosine of the angle between vectors a and b.
// Range: [-1, 1]; identical direction → 1, orthogonal → 0, opposite → -1.
//
// It returns 0 (rather than panicking or returning NaN) when the vectors
// differ in length, are empty, or either has zero magnitude — treating those
// as "no signal" so the retrieval/dedup paths that call it stay robust to a
// dimension switch (e.g. a 128d echo vector compared against a 768d real one).
//
// This is the exact general formula every prior private copy used; it does NOT
// assume the inputs are pre-normalized.
func Similarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
