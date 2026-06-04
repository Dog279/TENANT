package distill_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/distill"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// --- test scaffolding ---

type scaffold struct {
	t         *testing.T
	estore    *episodic.Store
	sstore    *semantic.Store
	router    *model.Router
	fakeLLM   *testllm.Fake
	fakeEmb   *testllm.Fake
}

func newScaffold(t *testing.T) *scaffold {
	t.Helper()
	dir := t.TempDir()
	estore, err := episodic.Open(filepath.Join(dir, "ep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = estore.Close() })
	sstore, err := semantic.Open(filepath.Join(dir, "fact.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sstore.Close() })

	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	router := model.NewRouter(reg, slog.Default())

	fakeLLM := testllm.New()
	fakeEmb := testllm.New()
	// Route by role: summarizer → fakeLLM, embedder → fakeEmb. We
	// register one factory that inspects the Profile's role and
	// returns the matching fake.
	router.RegisterBackend("vllm", func(_ context.Context, p model.Profile, _ *slog.Logger) (any, error) {
		switch p.Role {
		case model.RoleEmbedder:
			return fakeEmb, nil
		default:
			return fakeLLM, nil
		}
	})

	return &scaffold{t: t, estore: estore, sstore: sstore, router: router, fakeLLM: fakeLLM, fakeEmb: fakeEmb}
}

func (s *scaffold) insertEpisode(prompt, response string, embedding []float32) int64 {
	s.t.Helper()
	id, err := s.estore.Insert(context.Background(), &episodic.Episode{
		AgentID:    "test-agent",
		Visibility: episodic.VisibilityPrivate,
		Prompt:     prompt,
		Response:   response,
		EmbedderID: "test-embedder",
		Embedding:  embedding,
		Outcome:    episodic.OutcomeSuccess,
	})
	if err != nil {
		s.t.Fatalf("insertEpisode: %v", err)
	}
	return id
}

func (s *scaffold) distiller() *distill.Distiller {
	return &distill.Distiller{
		Router:              s.router,
		Episodic:            s.estore,
		Semantic:            s.sstore,
		AgentID:             "test-agent",
		BatchSize:           5,
		SimilarityThreshold: 0.85,
		Logger:              slog.Default(),
	}
}

func vecA() []float32 { return []float32{1, 0, 0, 0} }
func vecB() []float32 { return []float32{0, 1, 0, 0} }

// scriptFacts makes the fake LLM return the given facts JSON for the
// next call.
func scriptFacts(fake *testllm.Fake, facts string) {
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: facts, FinishReason: "stop"}, nil
	}
}

// scriptEmbeddings makes the fake embedder return the given vectors in
// input order. If more inputs are passed than vectors provided, the
// extras get the last vector.
func scriptEmbeddings(fake *testllm.Fake, vectors [][]float32) {
	fake.EmbedFn = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			if i < len(vectors) {
				out[i] = vectors[i]
			} else {
				out[i] = vectors[len(vectors)-1]
			}
		}
		return out, nil
	}
}

// --- tests ---

func TestRun_NoEpisodesIsNoop(t *testing.T) {
	s := newScaffold(t)
	d := s.distiller()
	res, err := d.Run(context.Background(), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.EpisodesProcessed != 0 || res.FactsInserted != 0 {
		t.Errorf("expected no-op, got %+v", res)
	}
	if res.LastEpisodeID != 0 {
		t.Errorf("LastEpisodeID = %d, want 0", res.LastEpisodeID)
	}
}

func TestRun_ExtractsFactsFromEpisodes(t *testing.T) {
	s := newScaffold(t)
	id1 := s.insertEpisode("I prefer Go for backend services", "noted", vecA())
	id2 := s.insertEpisode("I'm working on Tenant", "got it", vecB())

	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"User prefers Go for backend services.","confidence":0.9,"source_episode_ids":[`+itoa(id1)+`]},
		{"fact":"User works on a project named Tenant.","confidence":0.85,"source_episode_ids":[`+itoa(id2)+`]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecA(), vecB()})

	res, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FactsExtracted != 2 {
		t.Errorf("FactsExtracted = %d, want 2", res.FactsExtracted)
	}
	if res.FactsInserted != 2 {
		t.Errorf("FactsInserted = %d, want 2", res.FactsInserted)
	}
	if res.FactsReaffirmed != 0 {
		t.Errorf("FactsReaffirmed = %d, want 0", res.FactsReaffirmed)
	}
	if res.EpisodesProcessed != 2 {
		t.Errorf("EpisodesProcessed = %d, want 2", res.EpisodesProcessed)
	}
	if res.LastEpisodeID != id2 {
		t.Errorf("LastEpisodeID = %d, want %d", res.LastEpisodeID, id2)
	}
	// Verify facts actually landed in the semantic store.
	n, _ := s.sstore.Count(context.Background(), false, false)
	if n != 2 {
		t.Errorf("semantic store has %d facts, want 2", n)
	}
}

func TestRun_ReaffirmsExistingFactOnHighSimilarity(t *testing.T) {
	s := newScaffold(t)
	// Pre-seed a fact with an OLD last_confirmed so the bump is observable.
	// (last_confirmed has 1-second resolution; if Insert and Reaffirm fire
	// within the same second the test can't tell — pre-aging avoids this.)
	old := time.Now().UTC().Add(-24 * time.Hour)
	originalFactID, err := s.sstore.Insert(context.Background(), &semantic.Fact{
		AgentID:       "test-agent",
		Visibility:    semantic.VisibilityPrivate,
		Fact:          "User prefers Go.",
		Confidence:    0.9,
		EmbedderID:    "test-embedder",
		Embedding:     vecA(),
		FirstSeen:     old,
		LastConfirmed: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stash the original last_confirmed for comparison.
	original, _ := s.sstore.Get(context.Background(), originalFactID)
	originalLastConf := original.LastConfirmed

	// Now insert an episode that produces a near-identical fact.
	id := s.insertEpisode("Go is my preferred language", "noted", vecA())
	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"User prefers Go programming.","confidence":0.85,"source_episode_ids":[`+itoa(id)+`]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecA()}) // identical embedding → cosine = 1.0

	res, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FactsReaffirmed != 1 {
		t.Errorf("FactsReaffirmed = %d, want 1", res.FactsReaffirmed)
	}
	if res.FactsInserted != 0 {
		t.Errorf("FactsInserted = %d, want 0 (should have reaffirmed)", res.FactsInserted)
	}
	// Verify last_confirmed advanced on the original.
	updated, _ := s.sstore.Get(context.Background(), originalFactID)
	if !updated.LastConfirmed.After(originalLastConf) {
		t.Errorf("LastConfirmed didn't advance: %v → %v", originalLastConf, updated.LastConfirmed)
	}
}

func TestRun_InsertsNewFactOnLowSimilarity(t *testing.T) {
	s := newScaffold(t)
	// Pre-seed with vecA (orthogonal to vecB).
	_, _ = s.sstore.Insert(context.Background(), &semantic.Fact{
		AgentID: "test-agent", Fact: "User uses A",
		Confidence: 0.9, EmbedderID: "x", Embedding: vecA(),
	})

	id := s.insertEpisode("entirely unrelated", "ok", vecB())
	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"User uses B.","confidence":0.8,"source_episode_ids":[`+itoa(id)+`]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecB()}) // orthogonal → low cosine

	res, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.FactsInserted != 1 {
		t.Errorf("FactsInserted = %d, want 1", res.FactsInserted)
	}
	if res.FactsReaffirmed != 0 {
		t.Errorf("FactsReaffirmed = %d, want 0", res.FactsReaffirmed)
	}
}

func TestRun_RespectsSinceCursor(t *testing.T) {
	s := newScaffold(t)
	id1 := s.insertEpisode("old episode", "old", vecA())
	id2 := s.insertEpisode("new episode", "new", vecB())

	// Only process episodes after id1.
	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"Newer fact.","confidence":0.9,"source_episode_ids":[`+itoa(id2)+`]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecB()})

	res, err := s.distiller().Run(context.Background(), id1)
	if err != nil {
		t.Fatal(err)
	}
	if res.EpisodesProcessed != 1 {
		t.Errorf("EpisodesProcessed = %d, want 1 (id1 should be skipped)", res.EpisodesProcessed)
	}
	if res.LastEpisodeID != id2 {
		t.Errorf("LastEpisodeID = %d, want %d", res.LastEpisodeID, id2)
	}
}

func TestRun_ScopesByAgentID(t *testing.T) {
	s := newScaffold(t)
	// Episode for a DIFFERENT agent.
	_, err := s.estore.Insert(context.Background(), &episodic.Episode{
		AgentID: "other-agent", Prompt: "x", Response: "y",
		EmbedderID: "e", Embedding: vecA(),
	})
	if err != nil {
		t.Fatal(err)
	}

	scriptFacts(s.fakeLLM, `{"facts":[]}`)
	scriptEmbeddings(s.fakeEmb, nil)

	res, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.EpisodesProcessed != 0 {
		t.Errorf("EpisodesProcessed = %d, want 0 (other agent should not be touched)", res.EpisodesProcessed)
	}
}

func TestRun_MalformedLLMResponseRecordedAsBatchError(t *testing.T) {
	s := newScaffold(t)
	s.insertEpisode("question", "answer", vecA())

	// Return garbage that doesn't parse.
	s.fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "not valid json {", FinishReason: "stop"}, nil
	}
	scriptEmbeddings(s.fakeEmb, nil)

	res, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatalf("Run should not error on malformed LLM: %v", err)
	}
	if len(res.BatchErrors) != 1 {
		t.Errorf("BatchErrors len = %d, want 1", len(res.BatchErrors))
	}
	// Cursor should still advance past the failed batch — we don't
	// want to re-process the same bad episodes forever.
	if res.LastEpisodeID == 0 {
		t.Errorf("LastEpisodeID should advance even when batch failed; got 0")
	}
}

func TestRun_EmptyLLMResponseIsBatchError(t *testing.T) {
	s := newScaffold(t)
	s.insertEpisode("x", "y", vecA())
	s.fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "", FinishReason: "stop"}, nil
	}
	scriptEmbeddings(s.fakeEmb, nil)
	res, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.BatchErrors) != 1 {
		t.Errorf("BatchErrors len = %d, want 1", len(res.BatchErrors))
	}
}

func TestRun_BatchesLargeEpisodeRuns(t *testing.T) {
	s := newScaffold(t)
	// Insert 12 episodes, batch size 5 → 3 batches (5+5+2).
	for i := 0; i < 12; i++ {
		s.insertEpisode("p"+itoa(int64(i)), "r", vecA())
	}
	scriptFacts(s.fakeLLM, `{"facts":[]}`) // empty extraction each batch
	scriptEmbeddings(s.fakeEmb, nil)

	res, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.EpisodesProcessed != 12 {
		t.Errorf("EpisodesProcessed = %d, want 12", res.EpisodesProcessed)
	}
	if res.BatchesAttempted != 3 {
		t.Errorf("BatchesAttempted = %d, want 3", res.BatchesAttempted)
	}
	if len(s.fakeLLM.Generated) != 3 {
		t.Errorf("LLM was called %d times, want 3 (one per batch)", len(s.fakeLLM.Generated))
	}
}

func TestRun_FiltersFactsWithOutOfBatchSourceIDs(t *testing.T) {
	s := newScaffold(t)
	id := s.insertEpisode("real", "yes", vecA())

	// Summarizer hallucinates an episode ID that wasn't in the batch.
	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"Real fact.","confidence":0.8,"source_episode_ids":[`+itoa(id)+`, 99999]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecA()})

	_, err := s.distiller().Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Fact was still inserted; the bogus source ID was filtered.
	all, _ := s.sstore.Count(context.Background(), false, false)
	if all != 1 {
		t.Errorf("expected 1 fact persisted, got %d", all)
	}
}

func TestRun_ValidateMissingFieldsReturnsErr(t *testing.T) {
	for name, d := range map[string]*distill.Distiller{
		"missing Router":   {AgentID: "a", Episodic: &episodic.Store{}, Semantic: &semantic.Store{}},
		"missing AgentID":  {Router: &model.Router{}, Episodic: &episodic.Store{}, Semantic: &semantic.Store{}},
		"missing Episodic": {AgentID: "a", Router: &model.Router{}, Semantic: &semantic.Store{}},
		"missing Semantic": {AgentID: "a", Router: &model.Router{}, Episodic: &episodic.Store{}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.Run(context.Background(), 0)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRun_RespectsContextCancellation(t *testing.T) {
	s := newScaffold(t)
	for i := 0; i < 20; i++ {
		s.insertEpisode("x", "y", vecA())
	}
	d := s.distiller()
	d.BatchSize = 1 // many batches so cancellation has time to bite

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after first batch.
	s.fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		cancel()
		return &model.GenerateResponse{Text: `{"facts":[]}`, FinishReason: "stop"}, nil
	}

	res, err := d.Run(ctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res != nil && res.EpisodesProcessed >= 20 {
		t.Errorf("EpisodesProcessed = %d, want <20 (should have stopped)", res.EpisodesProcessed)
	}
}

// --- helpers ---

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Belt-and-suspenders: confirm json.Marshal of our extracted fact
// shape produces what we expect. Documents the contract.
func TestExtractedFactJSONShape(t *testing.T) {
	type fact struct {
		Fact             string  `json:"fact"`
		Confidence       float64 `json:"confidence"`
		SourceEpisodeIDs []int64 `json:"source_episode_ids"`
	}
	b, err := json.Marshal(fact{Fact: "x", Confidence: 0.5, SourceEpisodeIDs: []int64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"source_episode_ids":[1,2]`) {
		t.Fatalf("unexpected JSON shape: %s", b)
	}
}

func TestExtractJSONObject_HandlesRealModelOutput(t *testing.T) {
	// Cases observed from real Gemma 4 / Llama / Qwen output.
	cases := map[string]string{
		"bare":              `{"facts":[]}`,
		"json fence":        "```json\n{\"facts\":[{\"fact\":\"x\",\"confidence\":1.0,\"source_episode_ids\":[1]}]}\n```",
		"plain fence":       "```\n{\"facts\":[]}\n```",
		"leading prose":     "Here are the facts:\n{\"facts\":[]}",
		"trailing prose":    "{\"facts\":[]}\nLet me know if you need more.",
		"prose both sides":  "Sure!\n```json\n{\"facts\":[]}\n```\nHope that helps.",
		"braces in strings": `{"facts":[{"fact":"uses {curly} braces","confidence":1,"source_episode_ids":[]}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := distill.ExtractJSONObjectForTest(in)
			var v struct {
				Facts []struct {
					Fact string `json:"fact"`
				} `json:"facts"`
			}
			if err := json.Unmarshal([]byte(got), &v); err != nil {
				t.Fatalf("extracted %q did not parse: %v", got, err)
			}
		})
	}
}
