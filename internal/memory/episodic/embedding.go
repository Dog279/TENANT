package episodic

import (
	"encoding/binary"
	"errors"
	"math"
)

// Embeddings are stored as little-endian float32 BLOBs. Length is
// implicit from BLOB byte length / 4. Native endianness is endian-
// dependent across architectures; pinning to little-endian keeps DBs
// portable between, say, an x86_64 dev box and an ARM Mac Studio.

// encodeEmbedding serializes a vector to BLOB bytes.
func encodeEmbedding(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// decodeEmbedding parses a BLOB into a vector. Returns an error if
// the byte length is not a multiple of 4.
func decodeEmbedding(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("episodic: embedding BLOB length not multiple of 4")
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// cosineSimilarity returns the cosine of the angle between a and b.
// Range: [-1, 1]; identical direction → 1, orthogonal → 0, opposite → -1.
// Returns 0 if either vector is zero-magnitude or lengths differ
// (treating mismatched dims as "no signal" rather than crashing the
// search path).
func cosineSimilarity(a, b []float32) float64 {
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
