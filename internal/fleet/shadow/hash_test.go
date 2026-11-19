package shadow

import (
	"context"
	"database/sql"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustExec(t testing.TB, db *sql.DB, q string, args ...any) {
	t.Helper()
	res, err := db.Exec(q, args...)
	must(t, err)
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("%s changed no row", q)
	}
}

// Every way the stores write content, and deletion as privacy does it, changes
// the hash.
func TestContentHash_EveryContentWriteChangesIt(t *testing.T) {
	ctx := context.Background()
	later := longAgo.Add(48 * time.Hour)
	writes := map[string]func(p *testPair){
		"episode: insert":      func(p *testPair) { addEpisode(t, p.es, "uptime on web1", "up 41 days") },
		"episode: tombstone":   func(p *testPair) { must(t, p.es.Tombstone(ctx, 1)) },
		"episode: feedback":    func(p *testPair) { must(t, p.es.SetUserFeedback(ctx, 1, episodic.FeedbackAck)) },
		"episode: tags":        func(p *testPair) { must(t, p.es.AddTags(ctx, 2, []string{"host:web1"})) },
		"episode: delete":      func(p *testPair) { mustExec(t, p.ep.DB, `DELETE FROM episodes WHERE id = 1`) },
		"episode: text edited": func(p *testPair) { mustExec(t, p.ep.DB, `UPDATE episodes SET response = 'redacted' WHERE id = 2`) },
		"episode: one letter": func(p *testPair) {
			mustExec(t, p.ep.DB, `UPDATE episodes SET prompt = 'is nginx up on web2' WHERE id = 1`)
		},
		"episode: outcome":     func(p *testPair) { mustExec(t, p.ep.DB, `UPDATE episodes SET outcome = 'error' WHERE id = 1`) },
		"episode: tool calls":  func(p *testPair) { mustExec(t, p.ep.DB, `UPDATE episodes SET tool_calls = '[]' WHERE id = 1`) },
		"episode: run lineage": func(p *testPair) { mustExec(t, p.ep.DB, `UPDATE episodes SET source_run_id = 'r9' WHERE id = 1`) },
		"episode: visibility":  func(p *testPair) { mustExec(t, p.ep.DB, `UPDATE episodes SET visibility = 'shared' WHERE id = 1`) },
		"episode: null to ''":  func(p *testPair) { mustExec(t, p.ep.DB, `UPDATE episodes SET user_feedback = '' WHERE id = 1`) },

		"fact: insert":    func(p *testPair) { addFact(t, p.ss, "web1 has 16 GB of RAM") },
		"fact: supersede": func(p *testPair) { must(t, p.ss.Supersede(ctx, 2, 1)) },
		"fact: reaffirm":  func(p *testPair) { must(t, p.ss.Reaffirm(ctx, 2)) },
		"fact: tombstone": func(p *testPair) { must(t, p.ss.Tombstone(ctx, 2)) },
		"fact: edit": func(p *testPair) {
			_, err := p.ss.Edit(ctx, 2, "web1's /var fills monthly", "edit", "embedder", testVec(3))
			must(t, err)
		},
		"fact: pin":         func(p *testPair) { must(t, p.ss.SetPinned(ctx, 2, true)) },
		"fact: unpin":       func(p *testPair) { must(t, p.ss.SetPinned(ctx, 1, false)) },
		"fact: protect":     func(p *testPair) { must(t, p.ss.SetProtected(ctx, 2, true)) },
		"fact: valid until": func(p *testPair) { must(t, p.ss.SetValidTo(ctx, 2, later)) },
		"fact: importance":  func(p *testPair) { must(t, p.ss.ReinforceImportance(ctx, 2, 0.9)) },
		"fact: signals upsert": func(p *testPair) {
			must(t, p.ss.UpsertSignals(ctx, semantic.Signals{FactID: 2, Importance: 0.5, ProjectID: "ops"}))
		},
		"fact: delete":           func(p *testPair) { mustExec(t, p.sem.DB, `DELETE FROM facts WHERE id = 2`) },
		"fact: delete, cascaded": func(p *testPair) { mustExec(t, p.sem.DB, `DELETE FROM facts WHERE id = 1`) },
		"fact: source removed": func(p *testPair) {
			mustExec(t, p.sem.DB, `INSERT INTO fact_source_tombstones VALUES (1, 'episode', '7', 1700000000, 'forget')`)
		},
		"fact: origin":     func(p *testPair) { mustExec(t, p.sem.DB, `UPDATE fact_origin SET model = 'm2' WHERE fact_id = 1`) },
		"fact: confidence": func(p *testPair) { mustExec(t, p.sem.DB, `UPDATE facts SET confidence = 0.25 WHERE id = 1`) },
		"fact: sources":    func(p *testPair) { mustExec(t, p.sem.DB, `UPDATE facts SET source_episodes = '[1]' WHERE id = 1`) },
	}
	for name, write := range writes {
		p := someMemory(t)
		before := p.content(t)
		if again := p.content(t); again != before {
			t.Fatalf("%s: the hash of an untouched memory changed", name)
		}
		write(p)
		if p.content(t) == before {
			t.Errorf("%s: the hash did not change", name)
		}
	}

	// Two that need a first step: undoing a removal, and going back a version.
	p := someMemory(t)
	must(t, p.ss.Tombstone(ctx, 2))
	before := p.content(t)
	must(t, p.ss.Restore(ctx, 2))
	if p.content(t) == before {
		t.Error("fact: restore: the hash did not change")
	}
	newer, err := p.ss.Edit(ctx, 2, "another wording", "edit", "embedder", testVec(4))
	must(t, err)
	before = p.content(t)
	_, err = p.ss.RestoreVersion(ctx, 2, "embedder", testVec(5))
	must(t, err)
	if p.content(t) == before {
		t.Errorf("fact: restore version over #%d: the hash did not change", newer)
	}
}

// Recall and everything else that only counts accesses or rewrites vectors
// leaves the hash as it was.
func TestContentHash_RecallAndVectorsDoNotChangeIt(t *testing.T) {
	ctx := context.Background()
	p := someMemory(t)
	want := p.content(t)
	check := func(what string) {
		t.Helper()
		if got := p.content(t); got != want {
			t.Fatalf("%s changed the hash", what)
		}
	}
	// Recall as the agent does it: a search, then an access bump on each hit,
	// which makes a signals row for a fact that had none.
	facts, err := p.ss.Search(ctx, semantic.Query{Keywords: "web1", Embedding: testVec(2000), K: 10, Now: longAgo})
	must(t, err)
	if len(facts) != 2 {
		t.Fatalf("recall found %d facts, want 2", len(facts))
	}
	var hit []int64
	for _, h := range facts {
		hit = append(hit, h.Fact.ID)
	}
	must(t, p.ss.BumpAccess(ctx, hit))
	check("a recall")
	var rows int
	must(t, p.sem.QueryRow(`SELECT count(*) FROM fact_signals WHERE access_count = 1`).Scan(&rows))
	if rows != 2 {
		t.Fatalf("the access was counted on %d rows, want 2: the recall did not write", rows)
	}
	for range 5 {
		must(t, p.ss.BumpAccess(ctx, hit))
	}
	check("more access bumps")
	must(t, p.ss.DampenHeat(ctx, "main", 10))
	must(t, p.sem.QueryRow(`SELECT count(*) FROM fact_signals WHERE access_count = 3`).Scan(&rows))
	if rows != 2 {
		t.Fatalf("heat was dampened on %d rows, want 2", rows)
	}
	check("dampening heat")
	episodes, err := p.es.Search(ctx, episodic.Query{Keywords: "nginx", Embedding: testVec(1000), K: 10})
	must(t, err)
	if len(episodes) == 0 {
		t.Fatal("the episode search found nothing")
	}
	_, err = p.es.Recent(ctx, "main", 5)
	must(t, err)
	_, err = p.ss.List(ctx, semantic.ListFilter{})
	must(t, err)
	_, err = p.ss.PinnedFacts(ctx, "main", 5)
	must(t, err)
	check("reading")

	// Re-embedding, with another model at another width.
	must(t, p.es.UpdateEmbedding(ctx, 1, make([]float32, 768), "another-embedder"))
	must(t, p.ss.UpdateEmbedding(ctx, 1, make([]float32, 768), "another-embedder"))
	check("re-embedding")

	// Where SQLite keeps the rows: a copy, a compacted copy, the child's copy.
	ep, sem := p.images(t)
	if got := newTestPair(t, ep, sem).content(t); got != want {
		t.Fatal("a serialized copy hashes differently")
	}
	mustExec(t, p.ep.DB, `INSERT INTO episodes (agent_id, ts, prompt, response, embedder_id, embedding) VALUES ('main', 1, 'p', 'r', 'e', x'00')`)
	mustExec(t, p.ep.DB, `DELETE FROM episodes WHERE prompt = 'p'`)
	_, err = p.ep.Exec(`VACUUM`)
	must(t, err)
	_, err = p.sem.Exec(`VACUUM`)
	must(t, err)
	check("VACUUM")
	cep, err := withoutEmbeddings(ctx, ep)
	must(t, err)
	csem, err := withoutEmbeddings(ctx, sem)
	must(t, err)
	if got := newTestPair(t, cep, csem).content(t); got != want {
		t.Fatal("the copy without vectors hashes differently")
	}
}

// The hash separates values, so content cannot move between columns or rows
// and hash the same.
func TestContentHash_SeparatesValues(t *testing.T) {
	hashOf := func(prompt, response string, rows int) string {
		p := newTestPair(t, nil, nil)
		for range rows {
			addEpisode(t, p.es, prompt, response)
		}
		return p.content(t)
	}
	seen := map[string]string{}
	for name, h := range map[string]string{
		"ab|c":       hashOf("ab", "c", 1),
		"a|bc":       hashOf("a", "bc", 1),
		"abc|":       hashOf("abc", "", 1),
		"|abc":       hashOf("", "abc", 1),
		"ab|c twice": hashOf("ab", "c", 2),
		"empty":      hashOf("", "", 0),
	} {
		if other, dup := seen[h]; dup {
			t.Errorf("%q and %q hash the same", name, other)
		}
		seen[h] = name
	}
	// An empty store is not the same as no store, and the two stores do not
	// run together.
	a, b := newTestPair(t, nil, nil), newTestPair(t, nil, nil)
	h1, err := contentHash(context.Background(), a.ep.DB, a.sem.DB)
	must(t, err)
	h2, err := contentHash(context.Background(), a.ep.DB, b.sem.DB)
	must(t, err)
	if h1 != h2 || !validHash(h1) {
		t.Fatalf("two empty memories hash %s and %s", h1, h2)
	}
}

// schema is every table of a database and its columns, the FTS index's own
// tables left out.
func schema(t testing.TB, db *sql.DB) map[string][]string {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	must(t, err)
	defer tx.Rollback()
	tables, err := queryStrings(context.Background(), tx, `SELECT name FROM pragma_table_list WHERE schema = 'main' AND type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	must(t, err)
	out := map[string][]string{}
	for _, table := range tables {
		cols, err := queryStrings(context.Background(), tx, `SELECT name || ' ' || type FROM pragma_table_info(?) ORDER BY cid`, table)
		must(t, err)
		out[table] = cols
	}
	return out
}

// The hash, and what the child's copy leaves out, are written against the
// stores' columns as they are today. A new table or column is hashed without
// anyone deciding so, which is the safe way round, but a column that recall
// writes, or one that holds vectors, has to be added to unhashed or
// embeddingColumn by hand. This fails when the schema moves, to make someone
// look.
func TestContentHash_SchemaIsTheOneItWasWrittenFor(t *testing.T) {
	p := newTestPair(t, nil, nil)
	got := schema(t, p.ep.DB)
	maps.Copy(got, schema(t, p.sem.DB))
	want := map[string][]string{
		"episodes": {"id INTEGER", "agent_id TEXT", "visibility TEXT", "session_id TEXT", "source_run_id TEXT", "ts INTEGER",
			"prompt TEXT", "response TEXT", "tool_calls TEXT", "outcome TEXT", "user_feedback TEXT", "tags TEXT",
			"embedder_id TEXT", "embedding BLOB", "tombstoned INTEGER"},
		"facts": {"id INTEGER", "agent_id TEXT", "visibility TEXT", "fact TEXT", "source_episodes TEXT", "source_run_ids TEXT",
			"confidence REAL", "first_seen INTEGER", "last_confirmed INTEGER", "superseded_by INTEGER",
			"embedder_id TEXT", "embedding BLOB", "tombstoned INTEGER"},
		"fact_signals": {"fact_id INTEGER", "importance REAL", "confirm_count INTEGER", "access_count INTEGER",
			"last_accessed INTEGER", "pinned INTEGER", "protected INTEGER", "valid_from INTEGER", "valid_to INTEGER", "project_id TEXT"},
		"fact_source_tombstones": {"fact_id INTEGER", "source_kind TEXT", "source_id TEXT", "removed_at INTEGER", "reason TEXT"},
		"fact_origin":            {"fact_id INTEGER", "job TEXT", "model TEXT", "recorded_at INTEGER"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the stores' schema changed.\n got: %v\nwant: %v\nDecide whether the new column is content, "+
			"an access signal (unhashed, unhashedRows) or a vector (embeddingColumn), then update this test.", got, want)
	}
	// Everything the rules name exists, and every BLOB is a vector they cover.
	for table, cols := range unhashed {
		for _, c := range cols {
			if !slices.ContainsFunc(got[table], func(s string) bool { return strings.HasPrefix(s, c+" ") }) {
				t.Errorf("unhashed names %s.%s, which does not exist", table, c)
			}
		}
	}
	for table, cols := range got {
		for _, c := range cols {
			if name, isBlob := strings.CutSuffix(c, " BLOB"); isBlob && embeddingColumn[table] != name {
				t.Errorf("%s.%s is a BLOB that the child's copy would carry in full", table, name)
			}
		}
	}
}
