package privacy

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tenant/internal/memory/semantic"
	"time"
)

func (fx *fixture) db(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := openReadOnly(fx.d.path(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (fx *fixture) archiveText(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(filepath.Join(fx.d.DataDir, "archive"), func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			data, _ := os.ReadFile(p)
			b.Write(data)
		}
		return nil
	})
	return b.String()
}

func (fx *fixture) factSources(t *testing.T, name string) []int64 {
	t.Helper()
	var s sql.NullString
	if err := fx.db(t, factsDB).QueryRow(`SELECT source_episodes FROM facts WHERE id = ?`, fx.f[name]).Scan(&s); err != nil {
		t.Fatal(err)
	}
	ids, err := int64s(s)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func itemCount(items []Item, what string) int {
	for _, it := range items {
		if it.What == what || strings.HasPrefix(it.What, what+" (") {
			return it.Count
		}
	}
	return 0
}

func forget(t *testing.T, fx *fixture, sel Selector) (*Plan, *Result) {
	t.Helper()
	p, err := Forget(context.Background(), fx.d, sel)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p, res
}

func TestForgetTextRemovesItEverywhere(t *testing.T) {
	fx := newFixture(t)
	p, res := forget(t, fx, Selector{Match: "221b BAKER"})
	if n := itemCount(p.Items, "episodes (conversation turns)"); n != 1 {
		t.Fatalf("preview episodes = %d, want 1: %+v", n, p.Items)
	}
	eps := fx.db(t, episodesDB)
	if n := count(t, eps, `SELECT count(*) FROM episodes WHERE id = ?`, fx.ep["e1"]); n != 0 {
		t.Fatal("the matching episode survived")
	}
	if n := count(t, eps, `SELECT count(*) FROM episodes_fts WHERE episodes_fts MATCH 'baker'`); n != 0 {
		t.Fatal("the search index still finds the forgotten words")
	}
	facts := fx.db(t, factsDB)
	if n := count(t, facts, `SELECT count(*) FROM facts WHERE id = ?`, fx.f["f1"]); n != 0 {
		t.Fatal("the fact stating the address survived")
	}
	// f3 cited e1 and e2; it keeps e2 and records e1 as removed.
	if got := fx.factSources(t, "f3"); !slices.Equal(got, []int64{fx.ep["e2"]}) {
		t.Fatalf("f3 sources = %v, want [e2]", got)
	}
	st, err := semantic.Open(fx.d.path(factsDB))
	if err != nil {
		t.Fatal(err)
	}
	removed, err := st.RemovedSources(context.Background(), fx.f["f3"])
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"episode:" + idString(fx.ep["e1"]): true, "run:run-1": true}
	if len(removed) != 2 {
		t.Fatalf("removed sources = %+v, want e1 and run-1", removed)
	}
	for _, r := range removed {
		if !want[r.Kind+":"+r.ID] || r.Reason != "forget" || !r.RemovedAt.Equal(now) {
			t.Fatalf("unexpected removed source %+v", r)
		}
	}
	// The SME section cited f1 and f2; f2 remains, so it stays and drops f1.
	var cited string
	if err := facts.QueryRow(`SELECT source_fact_ids FROM sme_docs`).Scan(&cited); err != nil {
		t.Fatal(err)
	}
	if cited != "["+idString(fx.f["f2"])+"]" {
		t.Fatalf("sme citations = %s", cited)
	}
	sk := fx.db(t, skillsDB)
	if n := count(t, sk, `SELECT count(*) FROM skills WHERE name = 'address-lookup'`); n != 0 {
		t.Fatal("the skill mentioning the address survived")
	}
	if n := count(t, sk, `SELECT count(*) FROM skill_history WHERE instr(lower(prior_recipe), 'baker') > 0`); n != 0 {
		t.Fatal("an earlier skill version still holds the text")
	}
	if n := count(t, sk, `SELECT count(*) FROM skills WHERE name = 'weather-check'`); n != 1 {
		t.Fatal("a skill whose current version never mentioned the text was deleted")
	}
	if strings.Contains(strings.ToLower(fx.archiveText(t)), "baker") {
		t.Fatal("the archive still holds the text")
	}
	if !strings.Contains(fx.archiveText(t), "what's the weather") {
		t.Fatal("unrelated archive events were lost")
	}
	prof, _ := os.ReadFile(fx.d.path("profile.main.md"))
	if strings.Contains(strings.ToLower(string(prof)), "baker") || !strings.Contains(string(prof), "likes Go") {
		t.Fatalf("profile after scrub:\n%s", prof)
	}
	if n := count(t, fx.db(t, runsDB), `SELECT count(*) FROM runs WHERE trace_id = 'run-1'`); n != 0 {
		t.Fatal("the forgotten turn's run record survived")
	}
	found := false
	for _, n := range p.Notes {
		if strings.Contains(n, "main.toml") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no note about the soul mentioning it: %v", p.Notes)
	}
	if itemCount(res.Items, "facts") != 1 || itemCount(res.Items, "facts kept, with a removed source recorded") != 1 {
		t.Fatalf("result items: %+v", res.Items)
	}
}

func TestForgetSessionTakesItsDerivedFacts(t *testing.T) {
	fx := newFixture(t)
	forget(t, fx, Selector{Sessions: []string{"sess_b"}})
	if n := count(t, fx.db(t, episodesDB), `SELECT count(*) FROM episodes WHERE session_id = 'sess_b'`); n != 0 {
		t.Fatal("session episodes survived")
	}
	facts := fx.db(t, factsDB)
	if n := count(t, facts, `SELECT count(*) FROM facts WHERE id = ?`, fx.f["f2"]); n != 0 {
		t.Fatal("a fact derived only from the forgotten session survived (even pinned, a forget removes it)")
	}
	if n := count(t, facts, `SELECT count(*) FROM fact_signals WHERE fact_id = ?`, fx.f["f2"]); n != 0 {
		t.Fatal("the deleted fact's signals were left behind")
	}
	if n := count(t, facts, `SELECT count(*) FROM facts WHERE id = ?`, fx.f["f4"]); n != 1 {
		t.Fatal("a fact with no sources was touched")
	}
	matches, _ := filepath.Glob(filepath.Join(fx.d.DataDir, "archive", "*", "sess_b.jsonl"))
	if len(matches) != 0 {
		t.Fatalf("session archive file survived: %v", matches)
	}
	if n := count(t, fx.db(t, runsDB), `SELECT count(*) FROM runs WHERE session_id = 'sess_b'`); n != 0 {
		t.Fatal("session runs survived")
	}
}

func TestForgetOneEpisodeCutsJustItsTurn(t *testing.T) {
	fx := newFixture(t)
	forget(t, fx, Selector{Episodes: []int64{fx.ep["e2"]}})
	text := fx.archiveText(t)
	if strings.Contains(text, "what's the weather") || strings.Contains(text, "sunny") || strings.Contains(text, "weather near") {
		t.Fatalf("the forgotten turn's events remain:\n%s", text)
	}
	if !strings.Contains(text, "221B Baker") || !strings.Contains(text, "noted") {
		t.Fatal("the other turn in the same session was cut too")
	}
	if got := fx.factSources(t, "f3"); !slices.Equal(got, []int64{fx.ep["e1"]}) {
		t.Fatalf("f3 sources = %v, want [e1]", got)
	}
	sk := fx.db(t, skillsDB)
	var runs sql.NullString
	if err := sk.QueryRow(`SELECT source_run_ids FROM skills WHERE name = 'weather-check'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs.Valid {
		t.Fatalf("weather-check still cites the forgotten run: %s", runs.String)
	}
}

func TestForgetOneAgent(t *testing.T) {
	fx := newFixture(t)
	forget(t, fx, Selector{Agent: "researcher", Before: now})
	eps := fx.db(t, episodesDB)
	if n := count(t, eps, `SELECT count(*) FROM episodes WHERE agent_id = 'researcher'`); n != 0 {
		t.Fatal("the agent's episodes survived")
	}
	if n := count(t, eps, `SELECT count(*) FROM episodes WHERE agent_id = 'main'`); n != 3 {
		t.Fatalf("another agent's episodes were touched: %d left", n)
	}
	if _, err := os.Stat(fx.d.path("screenshots", "web-1.png")); err != nil {
		t.Fatal("an agent-scoped forget removed a file that belongs to no agent")
	}
	if !strings.Contains(fx.archiveText(t), "221B Baker") {
		t.Fatal("an agent-scoped forget removed another agent's archive events")
	}
}

func TestPreviewChangesNothing(t *testing.T) {
	fx := newFixture(t)
	snapshot := func() map[string][]byte {
		out := map[string][]byte{}
		_ = filepath.WalkDir(fx.d.DataDir, func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() && !strings.HasSuffix(p, "-shm") && !strings.HasSuffix(p, "-wal") {
				out[p], _ = os.ReadFile(p)
			}
			return nil
		})
		return out
	}
	before := snapshot()
	if _, err := Forget(context.Background(), fx.d, Selector{Match: "baker", Sessions: []string{"sess_b"}, Before: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(context.Background(), fx.d, Retention{Episodes: 1, Archive: 1, Facts: 1, Screenshots: 1}); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	for p, b := range before {
		if !bytes.Equal(b, after[p]) {
			t.Fatalf("a preview changed %s", p)
		}
	}
	if len(before) != len(after) {
		t.Fatalf("a preview added or removed files: %d -> %d", len(before), len(after))
	}
}

func TestApplySelectsAfresh(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	p, err := Forget(ctx, fx.d, Selector{Sessions: []string{"sess_b"}})
	if err != nil {
		t.Fatal(err)
	}
	// A new turn lands in the session between the preview and the apply.
	db, err := openStore(fx.d.path(episodesDB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO episodes (agent_id, session_id, ts, prompt, response, embedder_id, embedding) VALUES ('main', 'sess_b', ?, 'late', 'turn', 't/3', x'00')`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := p.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, fx.db(t, episodesDB), `SELECT count(*) FROM episodes WHERE session_id = 'sess_b'`); n != 0 {
		t.Fatal("a turn added after the preview survived the forget")
	}
}

func TestForgottenTextLeavesNoTraceInTheFiles(t *testing.T) {
	fx := newFixture(t)
	forget(t, fx, Selector{Match: "221B Baker"})
	for _, name := range []string{episodesDB, factsDB, skillsDB} {
		for _, suffix := range []string{"", "-wal"} {
			data, err := os.ReadFile(fx.d.path(name) + suffix)
			if err != nil {
				continue
			}
			if bytes.Contains(bytes.ToLower(data), []byte("221b baker")) {
				t.Fatalf("%s%s still holds the forgotten text on disk", name, suffix)
			}
		}
	}
}

func TestPruneKeepsFactsAndRecordsWhy(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	p, err := Prune(ctx, fx.d, Retention{Episodes: 90, Archive: 90, Screenshots: 30, EvalArtifacts: 30, Research: 30})
	if err != nil {
		t.Fatal(err)
	}
	if itemCount(p.Items, "episodes (conversation turns)") != 3 {
		t.Fatalf("preview: %+v", p.Items)
	}
	if _, err := p.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	eps := fx.db(t, episodesDB)
	if n := count(t, eps, `SELECT count(*) FROM episodes`); n != 1 {
		t.Fatalf("%d episodes left, want the September one", n)
	}
	facts := fx.db(t, factsDB)
	if n := count(t, facts, `SELECT count(*) FROM facts`); n != 4 {
		t.Fatalf("retention deleted facts: %d left", n)
	}
	if n := count(t, facts, `SELECT count(*) FROM fact_source_tombstones WHERE reason = 'retention'`); n == 0 {
		t.Fatal("no removed-source records for the pruned episodes")
	}
	months, _ := os.ReadDir(filepath.Join(fx.d.DataDir, "archive"))
	var names []string
	for _, m := range months {
		names = append(names, m.Name())
	}
	if !slices.Equal(names, []string{"2026-09"}) {
		t.Fatalf("archive months left: %v", names)
	}
	for _, gone := range []string{"screenshots/web-1.png", "eval-artifacts/eval-20260501.json", "research/20260501-120000-old"} {
		if _, err := os.Stat(fx.d.path(filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Fatalf("%s survived its retention", gone)
		}
	}
	for _, kept := range []string{"eval-artifacts/baseline.smoke.json", "eval-artifacts/trend.jsonl"} {
		if _, err := os.Stat(fx.d.path(filepath.FromSlash(kept))); err != nil {
			t.Fatalf("%s was pruned: %v", kept, err)
		}
	}
}

func TestPruneStaleFactsSparesPinned(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	p, err := Prune(ctx, fx.d, Retention{Facts: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	facts := fx.db(t, factsDB)
	if n := count(t, facts, `SELECT count(*) FROM facts WHERE id = ?`, fx.f["f2"]); n != 1 {
		t.Fatal("a pinned fact aged out")
	}
	if n := count(t, facts, `SELECT count(*) FROM facts`); n != 1 {
		t.Fatalf("stale unpinned facts left: %d facts remain", n)
	}
}

func TestNothingSelected(t *testing.T) {
	fx := newFixture(t)
	if _, err := Forget(context.Background(), fx.d, Selector{}); err == nil {
		t.Fatal("an empty selector was accepted")
	}
	if _, err := Forget(context.Background(), fx.d, Selector{Match: "   "}); err == nil {
		t.Fatal("a blank match was accepted")
	}
	if _, err := Forget(context.Background(), fx.d, Selector{ArchiveMonths: []string{"May"}}); err == nil {
		t.Fatal("a malformed month was accepted")
	}
	p, err := Prune(context.Background(), fx.d, Retention{})
	if err != nil || !p.Empty() {
		t.Fatalf("an empty retention planned changes: %+v %v", p, err)
	}
}

func TestForgetArchiveMonth(t *testing.T) {
	fx := newFixture(t)
	forget(t, fx, Selector{ArchiveMonths: []string{"2026-05"}})
	if _, err := os.Stat(filepath.Join(fx.d.DataDir, "archive", "2026-05")); !os.IsNotExist(err) {
		t.Fatal("the archive month survived")
	}
	if n := count(t, fx.db(t, episodesDB), `SELECT count(*) FROM episodes`); n != 4 {
		t.Fatal("forgetting an archive month removed episodes")
	}
}

// Forgetting both sources of an SME section removes the section version too.
func TestForgetEverySourceOfAnSMESection(t *testing.T) {
	fx := newFixture(t)
	forget(t, fx, Selector{Sessions: []string{"sess_a", "sess_b"}})
	if n := count(t, fx.db(t, factsDB), `SELECT count(*) FROM sme_docs`); n != 0 {
		t.Fatal("an SME section citing only forgotten facts survived")
	}
	if n := count(t, fx.db(t, factsDB), `SELECT count(*) FROM facts`); n != 1 {
		t.Fatal("only the source-less fact should remain")
	}
}

// A run can be forgotten on its own (its tool call mentions the text) while
// its episode stays; the episode then drops the dangling run id.
func TestForgetARunKeepsItsEpisodeButUnlinksIt(t *testing.T) {
	fx := newFixture(t)
	forget(t, fx, Selector{Match: "secret-project"})
	if n := count(t, fx.db(t, runsDB), `SELECT count(*) FROM runs WHERE trace_id = 'run-4'`); n != 0 {
		t.Fatal("the run whose tool call mentioned the text survived")
	}
	var run sql.NullString
	if err := fx.db(t, episodesDB).QueryRow(`SELECT source_run_id FROM episodes WHERE id = ?`, fx.ep["e4"]).Scan(&run); err != nil {
		t.Fatalf("the episode went with its run: %v", err)
	}
	if run.Valid {
		t.Fatalf("the episode still cites the forgotten run %q", run.String)
	}
}

// While a running Tenant holds a store open, its uncheckpointed WAL frames
// can still hold what was just forgotten. The forget truncates the WAL.
func TestForgetTruncatesTheWALOfAStoreInUse(t *testing.T) {
	fx := newFixture(t)
	live, err := sql.Open("sqlite", "file:"+fx.d.path(episodesDB)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	live.SetMaxOpenConns(1)
	if _, err := live.Exec(`INSERT INTO episodes (agent_id, session_id, ts, prompt, response, embedder_id, embedding) VALUES ('main', 'sess_x', ?, 'my passport is X1234567', 'ok', 't/3', x'00')`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	forget(t, fx, Selector{Match: "X1234567"})
	data, err := os.ReadFile(fx.d.path(episodesDB) + "-wal")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("X1234567")) {
		t.Fatal("the WAL of a store in use still holds the forgotten text")
	}
	main, _ := os.ReadFile(fx.d.path(episodesDB))
	if bytes.Contains(main, []byte("X1234567")) {
		t.Fatal("the database file still holds the forgotten text")
	}
}

// With many rows the full-text index spreads over segments; deleted words
// only leave them when the index is optimized.
func TestForgetPurgesTheSearchIndex(t *testing.T) {
	fx := newFixture(t)
	db, err := openStore(fx.d.path(episodesDB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO episodes (agent_id, session_id, ts, prompt, response, embedder_id, embedding) VALUES ('main', 'sess_y', ?, 'zanzibarquux is my password hint', 'ok', 't/3', x'00')`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if _, err := db.Exec(`INSERT INTO episodes (agent_id, session_id, ts, prompt, response, embedder_id, embedding) VALUES ('main', 'sess_y', ?, ?, 'ok', 't/3', x'00')`,
			now.Unix(), "filler turn number "+strings.Repeat("w", i%7)+" "+idString(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	forget(t, fx, Selector{Match: "zanzibarquux"})
	data, err := os.ReadFile(fx.d.path(episodesDB))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("zanzibarquux")) {
		t.Fatal("the search index still holds the forgotten word")
	}
}

// Attachments (TEN-493) follow the archive: retention prunes the old ones,
// and forgetting an archive month removes that month's folder.
func TestAttachmentsFollowTheArchive(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	may10 := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	old := fx.d.path("attachments", "2026-05", "aa.png")
	fresh := fx.d.path("attachments", "2026-09", "bb.png")
	for _, p := range []string{old, fresh} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, may10, may10); err != nil {
		t.Fatal(err)
	}
	p, err := Prune(ctx, fx.d, Retention{Archive: 90})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("an attachment older than the archive's retention survived")
	}
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Fatal("the emptied month folder was left behind")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a recent attachment was pruned: %v", err)
	}
	f, err := Forget(ctx, fx.d, Selector{ArchiveMonths: []string{"2026-09"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(fresh)); !os.IsNotExist(err) {
		t.Fatal("forgetting 2026-09 left its attachments")
	}
}
