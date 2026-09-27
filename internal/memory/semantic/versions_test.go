package semantic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var testVec = []float32{0.1, 0.2, 0.3}

func TestOriginIsRecordedAndRead(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	f := &Fact{AgentID: "main", Fact: "likes Go", EmbedderID: "t", Embedding: testVec, OriginJob: "distill", OriginModel: "glm-5"}
	id, err := s.Insert(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != id {
		t.Fatalf("Insert set the fact's ID to %d, returned %d", f.ID, id)
	}
	o, ok, err := s.Origin(ctx, id)
	if err != nil || !ok || o.Job != "distill" || o.Model != "glm-5" || o.RecordedAt.IsZero() {
		t.Fatalf("origin = %+v, %v, %v", o, ok, err)
	}
	old, err := s.Insert(ctx, &Fact{AgentID: "main", Fact: "from before origins", EmbedderID: "t", Embedding: testVec})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Origin(ctx, old); ok || err != nil {
		t.Fatalf("a fact without an origin reported one: %v %v", ok, err)
	}
}

func TestEditMakesANewVersionAndKeepsTheOld(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	v1, err := s.Insert(ctx, &Fact{AgentID: "main", Fact: "lives in Berlin", SourceEpisodes: []int64{7}, Confidence: 0.8,
		EmbedderID: "t", Embedding: testVec, FirstSeen: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned(ctx, v1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.BumpAccess(ctx, []int64{v1}); err != nil {
		t.Fatal(err)
	}
	v2, err := s.Edit(ctx, v1, "lives in Munich", "edit", "t", testVec)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.Get(ctx, v1)
	nw, _ := s.Get(ctx, v2)
	if old.SupersededBy != v2 || nw.Fact != "lives in Munich" || len(nw.SourceEpisodes) != 1 || nw.Confidence != 0.8 {
		t.Fatalf("after edit: old=%+v new=%+v", old, nw)
	}
	if sig, _ := s.GetSignals(ctx, v2); !sig.Pinned || sig.AccessCount != 0 || !sig.LastAccessed.IsZero() {
		t.Fatalf("new version signals = %+v; the pin carries over, the use count starts again", sig)
	}
	if o, ok, _ := s.Origin(ctx, v2); !ok || o.Job != "edit" {
		t.Fatalf("new version origin = %+v", o)
	}
	if _, err := s.Edit(ctx, v1, "anything", "edit", "t", testVec); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("editing a version that is no longer current = %v", err)
	}
	if _, err := s.Edit(ctx, v2, "lives in Munich", "edit", "t", testVec); err == nil {
		t.Fatal("an edit that changes nothing was accepted")
	}

	// Restore v1's text: a third version, history grows.
	v3, err := s.RestoreVersion(ctx, v1, "t", testVec)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := s.Versions(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 || versions[0].ID != v1 || versions[1].ID != v2 || versions[2].ID != v3 {
		t.Fatalf("versions = %v", ids(versions))
	}
	if versions[2].Fact != "lives in Berlin" || versions[2].SupersededBy != 0 {
		t.Fatalf("restored version = %+v", versions[2])
	}
	if o, _, _ := s.Origin(ctx, v3); o.Job != "restore" {
		t.Fatalf("restore origin = %+v", o)
	}
	if _, err := s.RestoreVersion(ctx, v3, "t", testVec); err == nil {
		t.Fatal("restored the version that is already current")
	}
	// Versions is the same set from any member.
	if vs, _ := s.Versions(ctx, v1); len(vs) != 3 {
		t.Fatalf("versions from the first = %v", ids(vs))
	}
}

func TestEditRefusesARemovedFact(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	id, err := s.Insert(ctx, &Fact{AgentID: "main", Fact: "x", EmbedderID: "t", Embedding: testVec})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Edit(ctx, id, "y", "edit", "t", testVec); err == nil || !strings.Contains(err.Error(), "is removed") {
		t.Fatalf("editing a removed fact = %v", err)
	}
}

func TestVersionsOfAMissingFact(t *testing.T) {
	if _, err := openTestStore(t).Versions(context.Background(), 404); !errors.Is(err, ErrNotFound) {
		t.Fatalf("versions of a missing fact = %v, want ErrNotFound", err)
	}
}

// With the current version removed, an earlier one can't be made current over
// it; the error says what to do instead.
func TestRestoreVersionAfterTheCurrentOneWasRemoved(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	v1, err := s.Insert(ctx, &Fact{AgentID: "main", Fact: "lives in Berlin", EmbedderID: "t", Embedding: testVec})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.Edit(ctx, v1, "lives in Munich", "edit", "t", testVec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tombstone(ctx, v2); err != nil {
		t.Fatal(err)
	}
	_, err = s.RestoreVersion(ctx, v1, "t", testVec)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("current version (#%d) was removed", v2)) {
		t.Fatalf("restore over a removed version = %v", err)
	}
}

func ids(fs []*Fact) []int64 {
	var out []int64
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

// Two edits of the same version at once: exactly one becomes current, and the
// other leaves nothing behind (no second live fact outside the chain).
func TestConcurrentEditsCannotFork(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.Insert(ctx, &Fact{AgentID: "main", Fact: "v0", EmbedderID: "t", Embedding: testVec})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	// Open a connection per editor first, so they race instead of queueing
	// behind connection setup.
	s.DB().SetMaxIdleConns(n)
	conns := make([]*sql.Conn, n)
	for i := range conns {
		if conns[i], err = s.DB().Conn(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range conns {
		c.Close()
	}
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		won   int
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.Edit(ctx, id, fmt.Sprintf("v%d", i+1), "edit", "t", testVec); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d of %d concurrent edits succeeded, want 1", won, n)
	}
	live, err := s.List(ctx, ListFilter{AgentIDs: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.Count(ctx, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || all != 2 {
		t.Fatalf("after the race: %d live, %d in all; want 1 and 2", len(live), all)
	}
}

// A fact and its origin land together: if the origin cannot be written, the
// fact is not either.
func TestInsertIsAtomicWithItsOrigin(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.DB().ExecContext(ctx, `DROP TABLE fact_origin`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, &Fact{AgentID: "main", Fact: "orphan", EmbedderID: "t", Embedding: testVec, OriginJob: "distill"}); err == nil {
		t.Fatal("insert succeeded without its origin table")
	}
	if n, err := s.Count(ctx, true, true); err != nil || n != 0 {
		t.Fatalf("a failed insert left %d fact(s) (%v)", n, err)
	}
}
