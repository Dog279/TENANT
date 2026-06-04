// Package testllm provides test doubles for the model.LLM and
// model.Embedder interfaces. Ship these as first-class artifacts (not
// in _test.go) so upper layers (memory, agent runtime) can take a
// dependency on them in their own tests without spinning up real vLLM.
package testllm

import (
	"context"
	"sync"

	"tenant/internal/model"
)

// Fake is a programmable LLM + Embedder. Configure GenerateFn /
// TokenCountFn / EmbedFn to drive behavior; default implementations
// return harmless empties. All call arguments are recorded for assertion.
type Fake struct {
	GenerateFn       func(ctx context.Context, req model.GenerateRequest) (*model.GenerateResponse, error)
	GenerateStreamFn func(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error)
	TokenCountFn     func(ctx context.Context, text string) (int, error)
	EmbedFn          func(ctx context.Context, texts []string) ([][]float32, error)

	mu          sync.Mutex
	Generated   []model.GenerateRequest
	Streamed    []model.GenerateRequest
	TokenCounted []string
	Embedded    [][]string
}

// New constructs a Fake with no-op defaults. Override fields before use.
func New() *Fake { return &Fake{} }

// Generate records the request and dispatches to GenerateFn if set,
// otherwise returns an empty response with FinishReason="stop".
func (f *Fake) Generate(ctx context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
	f.mu.Lock()
	f.Generated = append(f.Generated, req)
	f.mu.Unlock()
	if f.GenerateFn != nil {
		return f.GenerateFn(ctx, req)
	}
	return &model.GenerateResponse{FinishReason: "stop"}, nil
}

// GenerateStream records the request and dispatches to GenerateStreamFn,
// otherwise returns an immediately-closed channel.
func (f *Fake) GenerateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	f.mu.Lock()
	f.Streamed = append(f.Streamed, req)
	f.mu.Unlock()
	if f.GenerateStreamFn != nil {
		return f.GenerateStreamFn(ctx, req)
	}
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

// TokenCount records the input text and dispatches to TokenCountFn,
// otherwise returns a deterministic 1-token-per-4-chars estimate.
func (f *Fake) TokenCount(ctx context.Context, text string) (int, error) {
	f.mu.Lock()
	f.TokenCounted = append(f.TokenCounted, text)
	f.mu.Unlock()
	if f.TokenCountFn != nil {
		return f.TokenCountFn(ctx, text)
	}
	return (len(text) + 3) / 4, nil
}

// Embed records the inputs and dispatches to EmbedFn, otherwise returns
// deterministic 8-dim vectors derived from text length.
func (f *Fake) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.Embedded = append(f.Embedded, append([]string(nil), texts...))
	f.mu.Unlock()
	if f.EmbedFn != nil {
		return f.EmbedFn(ctx, texts)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		v[0] = float32(len(t))
		out[i] = v
	}
	return out, nil
}

// Reset clears recorded call history. Function overrides are preserved.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Generated = nil
	f.Streamed = nil
	f.TokenCounted = nil
	f.Embedded = nil
}
