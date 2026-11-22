package privacy

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// openMem is a store on a database in RAM with one connection, foreign keys
// and secure delete on: what a fleet machine's shadow is.
func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// ForgetOpen works on stores the caller has open: it deletes what the
// selector picks in them, the facts derived only from it, and leaves the
// rest; the keyword index forgets the words too.
func TestForgetOpenOnStoresInRAM(t *testing.T) {
	ctx := context.Background()
	edb, fdb := openMem(t), openMem(t)
	eps, err := episodic.OpenDB(edb)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := semantic.OpenDB(fdb)
	if err != nil {
		t.Fatal(err)
	}
	ins := func(session, run, text string) int64 {
		id, err := eps.Insert(ctx, &episodic.Episode{AgentID: "main", SessionID: session, SourceRunID: run, Prompt: "p", Response: text, EmbedderID: "e", Embedding: []float32{1}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	gone := ins("s1", "run-a", "the zebra password")
	byRun := ins("s2", "run-b", "a turn named by its run")
	kept := ins("s2", "run-c", "nothing to forget")
	fact := func(text string, from ...int64) {
		if _, err := facts.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, SourceEpisodes: from, EmbedderID: "e", Embedding: []float32{1}}); err != nil {
			t.Fatal(err)
		}
	}
	fact("derived only from the forgotten turn", gone)
	fact("derived from both", gone, kept)
	fact("a fact that says zebra")
	fact("unrelated")

	if _, err := ForgetOpen(ctx, edb, fdb, Selector{}, time.Now()); err == nil {
		t.Fatal("an empty selector was accepted")
	}
	if _, err := ForgetOpen(ctx, edb, fdb, Selector{Facts: []int64{1}, Skills: []string{"x"}}, time.Now()); err == nil {
		t.Fatal("a selector of the master's alone (fact ids, skills) was accepted")
	}
	res, err := ForgetOpen(ctx, edb, fdb, Selector{Match: "ZEBRA", Runs: []string{"run-b"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings: %v", res.Warnings)
	}
	left, err := eps.List(ctx, episodic.ListFilter{IncludeTombstoned: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != kept {
		t.Fatalf("episodes left: %d", len(left))
	}
	_ = byRun
	all, err := facts.List(ctx, semantic.ListFilter{IncludeTombstoned: true, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, f := range all {
		texts = append(texts, f.Fact)
	}
	if got := strings.Join(texts, "|"); strings.Contains(got, "zebra") || strings.Contains(got, "only from the forgotten") || !strings.Contains(got, "derived from both") || !strings.Contains(got, "unrelated") {
		t.Fatalf("facts left: %s", got)
	}
	if hits, err := eps.Search(ctx, episodic.Query{Keywords: "zebra"}); err != nil || len(hits) != 0 {
		t.Errorf("the keyword index still finds the word: %d %v", len(hits), err)
	}
	var n int
	if err := fdb.QueryRow(`SELECT count(*) FROM fact_source_tombstones`).Scan(&n); err != nil || n == 0 {
		t.Errorf("the fact that kept a source records the one it lost: %d %v", n, err)
	}
}
