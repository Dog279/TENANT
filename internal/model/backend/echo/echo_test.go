package echo_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/backend/echo"
)

func mk(t *testing.T) (model.LLM, model.Embedder) {
	t.Helper()
	any, err := echo.New(context.Background(), model.Profile{ID: "echo", Backend: "echo"}, nil)
	if err != nil {
		t.Fatalf("echo.New: %v", err)
	}
	return any.(model.LLM), any.(model.Embedder)
}

func TestGenerate_Deterministic(t *testing.T) {
	llm, _ := mk(t)
	req := model.GenerateRequest{Messages: []model.Message{{Role: "user", Content: "hello there"}}}
	a, _ := llm.Generate(context.Background(), req)
	b, _ := llm.Generate(context.Background(), req)
	if a.Text != b.Text {
		t.Fatalf("non-deterministic: %q != %q", a.Text, b.Text)
	}
	if a.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", a.FinishReason)
	}
	if !contains(a.Text, "hello there") {
		t.Errorf("reply should echo the user message: %q", a.Text)
	}
}

func TestGenerate_JSONSchemaReturnsValidEmptyFacts(t *testing.T) {
	llm, _ := mk(t)
	resp, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages:   []model.Message{{Role: "user", Content: "distill these"}},
		JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Facts []any `json:"facts"`
	}
	if err := json.Unmarshal([]byte(resp.Text), &parsed); err != nil {
		t.Fatalf("schema response not valid JSON: %v (%q)", err, resp.Text)
	}
	if len(parsed.Facts) != 0 {
		t.Errorf("echo should emit empty facts, got %v", parsed.Facts)
	}
}

func TestGenerateStream_EmitsThenCloses(t *testing.T) {
	llm, _ := mk(t)
	ch, err := llm.GenerateStream(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "stream me"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var sawFinish bool
	for c := range ch {
		if c.Error != nil {
			t.Fatalf("stream error: %v", c.Error)
		}
		text += c.Delta
		if c.FinishReason == "stop" {
			sawFinish = true
		}
	}
	if !sawFinish {
		t.Error("never saw terminal finish chunk")
	}
	if !contains(text, "stream me") {
		t.Errorf("streamed text missing input: %q", text)
	}
}

func TestTokenCount(t *testing.T) {
	llm, _ := mk(t)
	n, _ := llm.TokenCount(context.Background(), "abcdefgh") // 8 chars
	if n != 2 {
		t.Errorf("TokenCount = %d, want 2", n)
	}
}

func TestEmbed_DeterministicAndNormalized(t *testing.T) {
	_, emb := mk(t)
	v1, _ := emb.Embed(context.Background(), []string{"the quick brown fox"})
	v2, _ := emb.Embed(context.Background(), []string{"the quick brown fox"})
	if len(v1) != 1 || len(v1[0]) != echo.EmbedDim {
		t.Fatalf("dim = %d, want %d", len(v1[0]), echo.EmbedDim)
	}
	for i := range v1[0] {
		if v1[0][i] != v2[0][i] {
			t.Fatalf("non-deterministic embedding at %d", i)
		}
	}
	// L2 norm ~= 1.
	var n float64
	for _, x := range v1[0] {
		n += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(n)-1.0) > 1e-5 {
		t.Errorf("not unit-normalized: |v| = %v", math.Sqrt(n))
	}
}

func TestEmbed_SharedWordsHaveNonzeroCosine(t *testing.T) {
	_, emb := mk(t)
	out, _ := emb.Embed(context.Background(), []string{
		"I prefer Go for backend",
		"what language do I prefer",
		"completely unrelated sentence about cats",
	})
	cosShared := cosine(out[0], out[1])     // share "prefer", "i"
	cosUnrelated := cosine(out[0], out[2])  // share nothing meaningful
	if cosShared <= cosUnrelated {
		t.Errorf("word-overlap not reflected: shared=%v unrelated=%v", cosShared, cosUnrelated)
	}
	if cosShared <= 0 {
		t.Errorf("expected positive cosine for shared-word texts, got %v", cosShared)
	}
}

func TestEmbed_EmptyTextStableUnitVector(t *testing.T) {
	_, emb := mk(t)
	out, _ := emb.Embed(context.Background(), []string{"", "!!!"})
	for _, v := range out {
		var n float64
		for _, x := range v {
			n += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(n)-1.0) > 1e-5 {
			t.Errorf("empty/punct text not unit vector: |v|=%v", math.Sqrt(n))
		}
	}
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
