package semantic

import (
	"encoding/binary"
	"errors"
	"math"

	"tenant/internal/memory/cosine"
)

// Same little-endian float32 BLOB scheme as the episodic tier. Kept
// in-package rather than imported to avoid coupling — both tiers share
// a convention, not a code dependency. If we ever swap encoders we
// can do it per tier rather than ripple a shared helper.

func encodeEmbedding(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func decodeEmbedding(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("semantic: embedding BLOB length not multiple of 4")
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// cosineSimilarity is a thin alias over the shared internal/memory/cosine
// package (the single source of truth); kept named so search.go reads unchanged.
func cosineSimilarity(a, b []float32) float64 {
	return cosine.Similarity(a, b)
}
