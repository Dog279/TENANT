package shadow

import (
	"context"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// testPair is two in-memory stores, as a shadow has them.
type testPair struct {
	ep, sem *memDB
	es      *episodic.Store
	ss      *semantic.Store
}

// newTestPair opens stores on two serialized databases, or empty ones.
func newTestPair(t testing.TB, epImage, semImage []byte) *testPair {
	t.Helper()
	ctx := context.Background()
	ep, err := openMem(ctx, epImage, epImage != nil)
	if err != nil {
		t.Fatal(err)
	}
	sem, err := openMem(ctx, semImage, semImage != nil)
	if err != nil {
		t.Fatal(err)
	}
	es, err := episodic.OpenDB(ep.DB)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := semantic.OpenDB(sem.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = es.Close(), ss.Close() })
	return &testPair{ep: ep, sem: sem, es: es, ss: ss}
}

func (p *testPair) images(t testing.TB) (ep, sem []byte) {
	t.Helper()
	ep, _, err := p.ep.serialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sem, _, err = p.sem.serialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ep, sem
}

func (p *testPair) content(t testing.TB) string {
	t.Helper()
	h, err := contentHash(context.Background(), p.ep.DB, p.sem.DB)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// testVec is a small vector whose bytes are easy to find in an image.
func testVec(seed float32) []float32 {
	v := make([]float32, 32)
	for i := range v {
		v[i] = seed + float32(i)*0.03125
	}
	return v
}

var longAgo = time.Unix(1_600_000_000, 0)

func addEpisode(t testing.TB, es *episodic.Store, prompt, response string) int64 {
	t.Helper()
	id, err := es.Insert(context.Background(), &episodic.Episode{
		AgentID: "main", SessionID: "s1", Timestamp: longAgo, Prompt: prompt, Response: response,
		ToolCalls:  []episodic.ToolCallRef{{ID: "c1", Name: "fleet_exec", Arguments: `{"host":"web1"}`}},
		EmbedderID: "embedder", Embedding: testVec(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addFact(t testing.TB, ss *semantic.Store, text string) int64 {
	t.Helper()
	id, err := ss.Insert(context.Background(), &semantic.Fact{
		AgentID: "main", Fact: text, FirstSeen: longAgo, LastConfirmed: longAgo,
		EmbedderID: "embedder", Embedding: testVec(2000), OriginJob: "fleet_note",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// someMemory is a pair with a little of everything in it.
func someMemory(t testing.TB) *testPair {
	t.Helper()
	p := newTestPair(t, nil, nil)
	addEpisode(t, p.es, "is nginx up on web1", "nginx is active (running) since Tuesday")
	addEpisode(t, p.es, "free disk on web1", "/var is at 91%")
	a := addFact(t, p.ss, "web1 runs nginx 1.26 behind haproxy")
	addFact(t, p.ss, "web1's /var fills up every few weeks")
	if err := p.ss.SetPinned(context.Background(), a, true); err != nil {
		t.Fatal(err)
	}
	return p
}

func episodicQuery(keywords string) episodic.Query {
	return episodic.Query{Keywords: keywords, K: 5}
}
