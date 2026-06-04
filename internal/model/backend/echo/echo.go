// Package echo is a deterministic, dependency-free LLM + Embedder
// backend. It exists so the entire Tenant stack — agent loop, memory
// tiers, distillation, MCP server — runs end-to-end with no vLLM, no
// network, no GPU. Swap a profile's backend from "echo" to "vllm" and
// point Endpoint at real hardware; nothing else changes.
//
// Behavior:
//
//   - Generate: returns a deterministic reply derived from the last
//     user message. If the request carries a JSONSchema (distillation
//     constrains output), returns minimal valid JSON ({"facts":[]}) so
//     the pipeline runs end-to-end without inventing fake facts.
//
//   - Embed: feature-hashed bag-of-words vectors, L2-normalized. Same
//     text → same vector; texts sharing words have nonzero cosine, so
//     retrieval is genuinely meaningful in demos (not random noise).
//
//   - TokenCount: ~4 chars per token. Good enough for budget math.
//
// echo is real code (not _test), so the shipped binary can run a fully
// functional agent offline. It is the local-dev / CI backend.
package echo

import (
	"context"
	"hash/fnv"
	"math"
	"strings"

	"log/slog"

	"tenant/internal/model"
)

// EmbedDim is the dimensionality of echo embeddings. Small (good for
// fast brute-force cosine), fixed so stored vectors stay comparable.
const EmbedDim = 128

// Backend satisfies model.LLM and model.Embedder.
type Backend struct {
	profile model.Profile
}

// New is a model.BackendFactory: func(ctx, Profile, *slog.Logger) (any, error).
func New(_ context.Context, p model.Profile, _ *slog.Logger) (any, error) {
	return &Backend{profile: p}, nil
}

// Generate returns a deterministic response.
func (b *Backend) Generate(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
	// Distillation (and any structured-output caller) sets JSONSchema.
	// Emit minimal valid JSON for the facts schema so the distill
	// pipeline runs without a real model hallucinating facts.
	if len(req.JSONSchema) > 0 {
		return &model.GenerateResponse{
			Text:         `{"facts":[]}`,
			FinishReason: "stop",
			Usage:        model.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}, nil
	}

	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Content
			break
		}
	}
	reply := buildReply(lastUser, len(req.Tools))
	return &model.GenerateResponse{
		Text:         reply,
		FinishReason: "stop",
		Usage: model.Usage{
			PromptTokens:     tokens(joinContent(req.Messages)),
			CompletionTokens: tokens(reply),
			TotalTokens:      tokens(joinContent(req.Messages)) + tokens(reply),
		},
	}, nil
}

// GenerateStream emits the full Generate result as one chunk then a
// terminal chunk — enough to exercise the streaming consumer path.
func (b *Backend) GenerateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	resp, err := b.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.StreamChunk, 2)
	ch <- model.StreamChunk{Delta: resp.Text}
	ch <- model.StreamChunk{FinishReason: resp.FinishReason, Usage: &resp.Usage}
	close(ch)
	return ch, nil
}

// TokenCount approximates 1 token per 4 chars.
func (b *Backend) TokenCount(_ context.Context, text string) (int, error) {
	return tokens(text), nil
}

// Embed returns one feature-hashed vector per input. Deterministic and
// word-overlap-sensitive so semantic search is demonstrable.
func (b *Backend) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = featureHash(t, EmbedDim)
	}
	return out, nil
}

// --- helpers ---

func buildReply(userMsg string, nTools int) string {
	u := strings.TrimSpace(strings.ReplaceAll(userMsg, "\n", " "))
	if u == "" {
		return "echo: (no user content) — I'm the deterministic dev backend; wire a vLLM profile for real generation."
	}
	if len(u) > 280 {
		u = u[:280] + "..."
	}
	var b strings.Builder
	b.WriteString("echo[dev backend]: I received your message and the assembled memory context. ")
	b.WriteString("You said: \"")
	b.WriteString(u)
	b.WriteString("\". ")
	if nTools > 0 {
		b.WriteString("(")
		b.WriteString(itoa(nTools))
		b.WriteString(" tool(s) were available; the echo backend does not call tools — swap to vLLM for tool use.) ")
	}
	b.WriteString("This response is deterministic; replace the 'echo' profile backend with 'vllm' for a real model.")
	return b.String()
}

func joinContent(msgs []model.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

func tokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// featureHash maps tokens into a fixed-dim vector (the hashing trick),
// then L2-normalizes. Shared words → nonzero cosine similarity.
func featureHash(s string, dim int) []float32 {
	v := make([]float32, dim)
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		idx := int(h.Sum32() % uint32(dim))
		v[idx] += 1
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		// Empty / punctuation-only text: stable nonzero unit vector so
		// cosine is defined (avoids divide-by-zero downstream).
		v[0] = 1
		return v
	}
	inv := float32(1.0 / math.Sqrt(norm))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
