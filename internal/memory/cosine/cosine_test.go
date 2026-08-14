package cosine

import (
	"math"
	"testing"
)

// oldCosine is a verbatim copy of the private formula that used to live in
// episodic/semantic/distill/skills/wiki/assemble/consolidatejob/research/toolmux.
// The test below proves Similarity is byte-for-byte numerically equivalent so
// the consolidation is provably behavior-preserving.
func oldCosine(a, b []float32) float64 {
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

func TestSimilarity_EquivalentToOldImpl(t *testing.T) {
	cases := [][2][]float32{
		{{1, 0, 0}, {1, 0, 0}},  // identical → 1
		{{1, 0, 0}, {0, 1, 0}},  // orthogonal → 0
		{{1, 0, 0}, {-1, 0, 0}}, // opposite → -1
		{{1, 2, 3}, {4, 5, 6}},  // arbitrary
		{{0.1, -0.2, 0.3, 0.4}, {0.5, 0.6, -0.7, 0.8}},
		{{3.5, 1.25, -9.0}, {-2.0, 0.0, 4.5}},
		{{1, 2, 3}, {1, 2}},              // length mismatch → 0
		{{}, {}},                         // empty → 0
		{{0, 0, 0}, {1, 2, 3}},           // zero magnitude → 0
		{{1e-20, 1e-20}, {1e-20, 1e-20}}, // tiny magnitudes
	}
	for i, c := range cases {
		got := Similarity(c[0], c[1])
		want := oldCosine(c[0], c[1])
		if got != want {
			t.Fatalf("case %d: Similarity=%v old=%v (must be byte-identical)", i, got, want)
		}
	}
}

func TestSimilarity_KnownValues(t *testing.T) {
	if got := Similarity([]float32{1, 0, 0}, []float32{1, 0, 0}); math.Abs(got-1) > 1e-12 {
		t.Fatalf("identical vectors: got %v want 1", got)
	}
	if got := Similarity([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal: got %v want 0", got)
	}
	if got := Similarity([]float32{1, 0}, []float32{-1, 0}); math.Abs(got-(-1)) > 1e-12 {
		t.Fatalf("opposite: got %v want -1", got)
	}
}
