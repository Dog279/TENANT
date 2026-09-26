package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/datalock"
)

// tree is a small Tenant install: a config dir and a data dir with one of
// everything a backup has to handle.
type tree struct {
	root, cfg, data string
	secrets         []string
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeDB(t *testing.T, path, ddl string, rows ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(r); err != nil {
			t.Fatal(err)
		}
	}
}

func newTree(t *testing.T, shared bool) *tree {
	t.Helper()
	root := t.TempDir()
	tr := &tree{root: root, cfg: filepath.Join(root, "config"), data: filepath.Join(root, "data")}
	if shared {
		tr.cfg = tr.data
	}
	write(t, filepath.Join(tr.cfg, "config.json"), `{"schema_version":2}`)
	write(t, filepath.Join(tr.cfg, "soul", "main.toml"), "[meta]\nversion = 3\n")
	write(t, filepath.Join(tr.cfg, "credentials.json"), `{"secrets":{"openai":"sk-test"}}`)
	write(t, filepath.Join(tr.data, "x-token.json"), `{"access":"tok"}`)
	tr.secrets = []string{filepath.Join(tr.cfg, "credentials.json"), filepath.Join(tr.data, "x-token.json")}
	makeDB(t, filepath.Join(tr.data, "episodes.db"), `CREATE TABLE episodes (id INTEGER PRIMARY KEY, prompt TEXT)`,
		`INSERT INTO episodes (prompt) VALUES ('hello')`)
	makeDB(t, filepath.Join(tr.data, "facts.db"), `CREATE TABLE facts (id INTEGER PRIMARY KEY, body TEXT, source_episodes TEXT)`,
		`INSERT INTO facts (body, source_episodes) VALUES ('likes Go', '1')`)
	write(t, filepath.Join(tr.data, "archive", "2026-09", "sess_a.jsonl"), "{\"role\":\"user\"}\n{\"role\":\"assistant\"}\n{\"torn")
	write(t, filepath.Join(tr.data, "logs", "tui", "2026-09-26.log"), "plain text, not json\n")
	write(t, filepath.Join(tr.data, "research", "r1", "report.md"), "# Report\n")
	write(t, filepath.Join(tr.data, "wiki", "a1b2c3d4e5f60718.json"), `{"format":3}`)
	write(t, filepath.Join(tr.data, "episodes.db-journal"), "stale journal")
	write(t, filepath.Join(tr.data, "config.json.123.tmp"), "half-written")
	lock, err := datalock.Shared(tr.data)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
	return tr
}

func (tr *tree) opts(keys Keys) Options {
	return Options{
		ConfigDir: tr.cfg,
		DataDir:   tr.data,
		Secrets:   tr.secrets,
		ConfigFile: func(rel string) bool {
			return rel == "config.json" || rel == "credentials.json" || strings.HasPrefix(rel, "soul/")
		},
		Keys: keys,
		Now:  func() time.Time { return time.Date(2026, 9, 26, 15, 4, 5, 0, time.UTC) },
	}
}

func (tr *tree) restoreOpts(keys Keys) RestoreOptions {
	return RestoreOptions{
		ConfigDir: tr.cfg,
		DataDir:   tr.data,
		Keys:      keys,
		Secrets:   tr.secrets,
		Now:       func() time.Time { return time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC) },
	}
}

func entryPaths(m *Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		out = append(out, e.Path)
	}
	return out
}

func skippedPaths(m *Manifest) []string {
	var out []string
	for _, s := range m.Skipped {
		out = append(out, s.Path)
	}
	return out
}

func TestCreateCapturesTheRightFilesInReferenceOrder(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	res, err := Create(ctx, filepath.Join(tr.root, "out", "b.tar.gz"), tr.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	got := entryPaths(res.Manifest)
	for _, want := range []string{"config/config.json", "config/soul/main.toml", "data/episodes.db", "data/facts.db",
		"data/archive/2026-09/sess_a.jsonl", "data/logs/tui/2026-09-26.log", "data/research/r1/report.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("backup is missing %s (has %v)", want, got)
		}
	}
	for _, never := range []string{"config/credentials.json", "data/x-token.json", "data/.tenant.lock",
		"data/episodes.db-journal", "data/config.json.123.tmp", "data/wiki/a1b2c3d4e5f60718.json"} {
		if slices.Contains(got, never) {
			t.Errorf("backup carries %s", never)
		}
	}
	skipped := skippedPaths(res.Manifest)
	for _, want := range []string{"config/credentials.json", "data/x-token.json", "data/wiki/a1b2c3d4e5f60718.json"} {
		if !slices.Contains(skipped, want) {
			t.Errorf("%s is not listed as skipped (%v)", want, skipped)
		}
	}
	idx := func(p string) int { return slices.Index(got, p) }
	if !(idx("data/facts.db") < idx("data/episodes.db") && idx("data/episodes.db") < idx("data/archive/2026-09/sess_a.jsonl")) {
		t.Fatalf("capture order breaks references: %v", got)
	}
	for _, e := range res.Manifest.Entries {
		if e.Path == "data/archive/2026-09/sess_a.jsonl" && e.Size != int64(len("{\"role\":\"user\"}\n{\"role\":\"assistant\"}\n")) {
			t.Fatalf("archive copy kept the torn tail: %d bytes", e.Size)
		}
	}
	if res.Manifest.Secrets || res.Manifest.Encrypted {
		t.Fatalf("unencrypted backup flagged: %+v", res.Manifest)
	}
	// Create's staging dir is gone.
	entries, _ := os.ReadDir(tr.data)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Fatalf("staging dir left behind: %s", e.Name())
		}
	}
	rep, err := Verify(ctx, res.Path, Keys{}, Checks{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("fresh backup failed verify: %v", rep.Problems)
	}
}

func TestCreateRefusesBadLayouts(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	if _, err := Create(ctx, filepath.Join(tr.data, "b.tar.gz"), tr.opts(Keys{})); err == nil {
		t.Fatal("wrote a backup inside the data dir")
	}
	nested := tr.opts(Keys{})
	nested.DataDir = filepath.Join(tr.cfg, "data")
	write(t, filepath.Join(nested.DataDir, "episodes.db"), "")
	if _, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), nested); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("accepted a data dir nested in the config dir: %v", err)
	}
	nested.ConfigDir, nested.DataDir = nested.DataDir, tr.cfg
	if _, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), nested); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("accepted a config dir nested in the data dir: %v", err)
	}
	out := filepath.Join(tr.root, "exists.tar.gz")
	write(t, out, "precious")
	if _, err := Create(ctx, out, tr.opts(Keys{})); err == nil {
		t.Fatal("overwrote an existing file")
	}
	if b, _ := os.ReadFile(out); string(b) != "precious" {
		t.Fatal("existing file changed")
	}
}

func TestEncryptedRoundTripRestoresEverythingIncludingSecrets(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	keys := Keys{Passphrase: "pw", scryptWorkFactor: 10}
	res, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz.age"), tr.opts(keys))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Manifest.Secrets || !slices.Contains(entryPaths(res.Manifest), "config/credentials.json") {
		t.Fatalf("encrypted backup lacks secrets: %v", entryPaths(res.Manifest))
	}
	// Change the live install after the backup.
	write(t, filepath.Join(tr.cfg, "config.json"), `{"schema_version":2,"changed":true}`)
	write(t, filepath.Join(tr.cfg, "credentials.json"), `{"secrets":{"openai":"sk-rotated"}}`)
	write(t, filepath.Join(tr.data, "research", "r2", "report.md"), "# newer\n")

	if _, err := Restore(ctx, res.Path, tr.restoreOpts(Keys{})); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("restore without the passphrase: err = %v", err)
	}
	out, err := Restore(ctx, res.Path, tr.restoreOpts(Keys{Passphrase: "pw"}))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Restored || len(out.Aside) != 2 {
		t.Fatalf("restore result: %+v", out)
	}
	if b, _ := os.ReadFile(filepath.Join(tr.cfg, "config.json")); string(b) != `{"schema_version":2}` {
		t.Fatalf("config.json = %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(tr.cfg, "credentials.json")); !strings.Contains(string(b), "sk-test") {
		t.Fatalf("credentials not restored from the backup: %s", b)
	}
	if _, err := os.Stat(filepath.Join(tr.data, "research", "r2")); !os.IsNotExist(err) {
		t.Fatal("restore kept a file made after the backup")
	}
	if b, _ := os.ReadFile(filepath.Join(out.Aside[0], "config.json")); !strings.Contains(string(b), "changed") {
		t.Fatal("the replaced config dir was not kept aside")
	}
	db, err := openReadOnly(filepath.Join(tr.data, "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var body string
	if err := db.QueryRow(`SELECT body FROM facts`).Scan(&body); err != nil || body != "likes Go" {
		t.Fatalf("restored facts.db: %q, %v", body, err)
	}
}

func TestPlainRestoreKeepsTheLiveSecrets(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	res, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), tr.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tr.cfg, "credentials.json"), `{"secrets":{"openai":"sk-current"}}`)
	out, err := Restore(ctx, res.Path, tr.restoreOpts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(tr.cfg, "credentials.json")); !strings.Contains(string(b), "sk-current") {
		t.Fatalf("live credentials lost on a restore of a backup without secrets: %s", b)
	}
	if !slices.Contains(out.KeptSecrets, "config/credentials.json") || !slices.Contains(out.KeptSecrets, "data/x-token.json") {
		t.Fatalf("KeptSecrets = %v", out.KeptSecrets)
	}
}

func TestRestoreRefusesWhileTenantRuns(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	res, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), tr.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	running, err := datalock.Shared(tr.data)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Restore(ctx, res.Path, tr.restoreOpts(Keys{}))
	running.Release()
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("restore under a running Tenant: err = %v", err)
	}
	assertNoRestoreLeftovers(t, tr)
}

func assertNoRestoreLeftovers(t *testing.T, tr *tree) {
	t.Helper()
	entries, err := os.ReadDir(tr.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".restore-") || strings.Contains(e.Name(), ".pre-restore-") {
			t.Fatalf("left behind %s", e.Name())
		}
	}
}

func TestDryRunAndDamageChangeNothing(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	res, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), tr.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tr.cfg, "config.json"), `{"live":true}`)
	opts := tr.restoreOpts(Keys{})
	opts.DryRun = true
	out, err := Restore(ctx, res.Path, opts)
	if err != nil || out.Restored {
		t.Fatalf("dry run: %+v, %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(tr.cfg, "config.json")); string(b) != `{"live":true}` {
		t.Fatal("dry run changed the live config")
	}
	assertNoRestoreLeftovers(t, tr)

	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	damaged := filepath.Join(tr.root, "damaged.tar.gz")
	write(t, damaged, string(data))
	rep, err := Verify(ctx, damaged, Keys{}, Checks{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("verify passed a damaged backup")
	}
	if _, err := Restore(ctx, damaged, tr.restoreOpts(Keys{})); err == nil {
		t.Fatal("restored a damaged backup")
	}
	if b, _ := os.ReadFile(filepath.Join(tr.cfg, "config.json")); string(b) != `{"live":true}` {
		t.Fatal("a failed restore changed the live config")
	}
	assertNoRestoreLeftovers(t, tr)
}

func TestNewerSchemaNeedsForce(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	res, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), tr.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	// This "older" Tenant knows facts without source_episodes.
	checks := Checks{ReferenceSchema: func(_ context.Context, p string) (Schema, bool, error) {
		switch p {
		case "data/facts.db":
			return Schema{"facts": {"body", "id"}}, true, nil
		case "data/episodes.db":
			return Schema{"episodes": {"id", "prompt"}}, true, nil
		}
		return nil, false, nil
	}}
	rep, err := Verify(ctx, res.Path, Keys{}, checks, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || len(rep.Newer) != 1 || !strings.Contains(rep.Newer[0], "facts.source_episodes") {
		t.Fatalf("report: problems=%v newer=%v", rep.Problems, rep.Newer)
	}
	opts := tr.restoreOpts(Keys{})
	opts.Checks = checks
	if _, err := Restore(ctx, res.Path, opts); !errors.Is(err, ErrNewer) {
		t.Fatalf("restore of a newer backup: err = %v", err)
	}
	assertNoRestoreLeftovers(t, tr)
	opts.Force = true
	if out, err := Restore(ctx, res.Path, opts); err != nil || !out.Restored {
		t.Fatalf("forced restore: %+v, %v", out, err)
	}
}

func TestSharedDirBackupRestoresOntoSeparateDirs(t *testing.T) {
	ctx := context.Background()
	src := newTree(t, true) // the macOS default: config and data are one directory
	res, err := Create(ctx, filepath.Join(src.root, "b.tar.gz"), src.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	got := entryPaths(res.Manifest)
	if !slices.Contains(got, "config/config.json") || !slices.Contains(got, "data/episodes.db") {
		t.Fatalf("shared dir not split into roots: %v", got)
	}
	dst := &tree{root: t.TempDir()}
	dst.cfg, dst.data = filepath.Join(dst.root, "cfg"), filepath.Join(dst.root, "dat")
	out, err := Restore(ctx, res.Path, RestoreOptions{ConfigDir: dst.cfg, DataDir: dst.data})
	if err != nil || !out.Restored {
		t.Fatalf("restore: %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dst.cfg, "config.json")); err != nil {
		t.Fatalf("config.json not in the config dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst.data, "episodes.db")); err != nil {
		t.Fatalf("episodes.db not in the data dir: %v", err)
	}
	// And back onto one shared directory.
	res2, err := Create(ctx, filepath.Join(dst.root, "b2.tar.gz"), Options{ConfigDir: dst.cfg, DataDir: dst.data})
	if err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(t.TempDir(), "tenant")
	if out, err := Restore(ctx, res2.Path, RestoreOptions{ConfigDir: one, DataDir: one}); err != nil || !out.Restored {
		t.Fatalf("restore onto one dir: %+v, %v", out, err)
	}
	for _, f := range []string{"config.json", "episodes.db", "soul/main.toml"} {
		if _, err := os.Stat(filepath.Join(one, filepath.FromSlash(f))); err != nil {
			t.Fatalf("%s missing after restore onto one dir: %v", f, err)
		}
	}
}

func TestCreateWhileAStoreIsOpenForWriting(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	// A running Tenant keeps its stores open in WAL mode, with -wal and -shm
	// files beside them and commits landing in the WAL.
	live, err := sql.Open("sqlite", "file:"+filepath.Join(tr.data, "episodes.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.Exec(`INSERT INTO episodes (prompt) VALUES ('committed in the wal')`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tr.data, "episodes.db-wal")); err != nil {
		t.Fatalf("fixture: no WAL file while the store is open: %v", err)
	}
	res, err := Create(ctx, filepath.Join(tr.root, "b.tar.gz"), tr.opts(Keys{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range entryPaths(res.Manifest) {
		if strings.HasSuffix(p, "-wal") || strings.HasSuffix(p, "-shm") {
			t.Fatalf("backup carries a SQLite side file: %s", p)
		}
	}
	dst := &tree{root: t.TempDir()}
	dst.cfg, dst.data = filepath.Join(dst.root, "cfg"), filepath.Join(dst.root, "dat")
	if _, err := Restore(ctx, res.Path, RestoreOptions{ConfigDir: dst.cfg, DataDir: dst.data}); err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(filepath.Join(dst.data, "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM episodes WHERE prompt = 'committed in the wal'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a commit still in the WAL did not reach the backup: n=%d err=%v", n, err)
	}
	// The live store is still usable by its writer.
	if _, err := live.Exec(`INSERT INTO episodes (prompt) VALUES ('after')`); err != nil {
		t.Fatalf("live store broken by the backup: %v", err)
	}
}
