package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/config"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The key is created once, owner-only, and read back the same; a damaged key
// file is an error, never a new key.
func TestChainKeyIsCreatedOnce(t *testing.T) {
	cfg := t.TempDir()
	k1, err := chainKey(cfg)
	if err != nil || len(k1) != 32 {
		t.Fatalf("chainKey = %x, %v", k1, err)
	}
	k2, err := chainKey(cfg)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("second chainKey = %x, %v; want the first", k2, err)
	}
	if fi, err := os.Stat(chainKeyPath(cfg)); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v, %v", fi, err)
	}
	if managed, _ := managedSecrets(cfg, ""); !slices.ContainsFunc(managed, func(m managedSecret) bool { return m.path == chainKeyPath(cfg) }) {
		t.Error("the key is not a managed secret: doctor, the file tools and backups don't know it")
	}
	if err := os.WriteFile(chainKeyPath(cfg), []byte("not hex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := chainKey(cfg); err == nil {
		t.Error("a damaged key file was accepted")
	}
}

// Sandboxed commands and MCP servers are denied the key and the anchor.
func TestSandboxesAreDeniedTheChainFiles(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	plan, err := planMCPServer(config.MCPServer{Name: "files", Command: []string{"server"}}, cfg, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, deny := range [][]string{resolveExecProfile(surfaceLocal, cfg, data, "").Policy.DenyRoots, plan.Policy.DenyRoots} {
		for _, want := range []string{chainKeyPath(cfg), chainAnchorPath(cfg)} {
			if !slices.Contains(deny, want) {
				t.Errorf("deny roots %v lack %s", deny, want)
			}
		}
	}
}

func auditRecords(t *testing.T, st *store.Store, n int) {
	t.Helper()
	batch := make([]event.Event, n)
	for i := range batch {
		batch[i] = event.Event{Log: store.SecurityLog, Provider: "security", Level: event.Info, Msg: "audit " + strconv.Itoa(i)}
	}
	if err := st.Commit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
}

// The anchor task writes the head to <config>/logbook.anchor and the file
// witness at first, then every 1,000 records or every hour; it never moves
// the anchor off a head the chain no longer holds, and says so once.
func TestAnchorTask(t *testing.T) {
	ctx := context.Background()
	cfg := t.TempDir()
	witness := filepath.Join(t.TempDir(), "witness.json")
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2,"logs":{"security":{"witness":"`+filepath.ToSlash(witness)+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := chainKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "logbook.db"), store.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := logs.NewMemory(0)
	task := anchorChain(h, st, cfg)
	anchor := func() store.Head {
		t.Helper()
		a, err := readChainHead(chainAnchorPath(cfg))
		if err != nil || a == nil {
			t.Fatalf("anchor: %v, %v", a, err)
		}
		return *a
	}
	if err := task(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chainAnchorPath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("an empty chain was anchored: %v", err)
	}

	auditRecords(t, st, 3)
	if err := task(ctx); err != nil {
		t.Fatal(err)
	}
	if a, w := anchor(), chainHeadIn(t, witness); a.Rec != 3 || a.Time.IsZero() || w == nil || w.Rec != a.Rec || w.Hash != a.Hash || !w.Time.Equal(a.Time) {
		t.Fatalf("first anchor %+v, witness %+v", a, w)
	}
	auditRecords(t, st, 500)
	if err := task(ctx); err != nil || anchor().Rec != 3 {
		t.Fatalf("anchored again after 500 records within the hour: %+v, %v", anchor(), err)
	}
	auditRecords(t, st, 500)
	if err := task(ctx); err != nil || anchor().Rec != 1003 {
		t.Fatalf("not anchored after 1,000 records: %+v, %v", anchor(), err)
	}
	auditRecords(t, st, 1)
	a := anchor()
	a.Time = a.Time.Add(-2 * time.Hour)
	if err := writeChainAnchor(cfg, a); err != nil {
		t.Fatal(err)
	}
	if err := task(ctx); err != nil || anchor().Rec != 1004 {
		t.Fatalf("not anchored after an hour: %+v, %v", anchor(), err)
	}

	// The database lost the anchored record (an older copy put back): the
	// anchor stays, and one critical event says why.
	stale := store.Head{Rec: 5000, Hash: strings.Repeat("ab", 32), Time: time.Now().Add(-2 * time.Hour)}
	if err := writeChainAnchor(cfg, stale); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := task(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := anchor(); got.Rec != stale.Rec || got.Hash != stale.Hash {
		t.Errorf("the anchor moved off a head the chain lost: %+v", got)
	}
	entries, _ := h.Snapshot()
	n := 0
	for _, e := range entries {
		if e.Level == logs.LevelCritical && strings.Contains(e.Msg, "no longer holds its anchor") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d critical events for the lost anchor, want 1: %+v", n, entries)
	}

	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2,"logs":{"security":{"witness":"peer:office"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := witnessChain(cfg, a); err == nil || !strings.Contains(err.Error(), "peer") {
		t.Errorf("a peer witness: %v", err)
	}
}

func chainHeadIn(t *testing.T, path string) *store.Head {
	t.Helper()
	h, err := readChainHead(path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// `tenant backup restore` writes a re-anchor record and moves the anchor to
// it, so the restored Security log verifies as a restore, naming the head it
// replaced and the backup, rather than as a rollback.
func TestBackupRestoreReanchors(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	c := &commonFlags{cfgDir: cfg, dataDir: data}
	key, err := chainKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "logs", "logbook.db")
	commit := func(n int) store.Head {
		st, err := store.Open(path, store.Options{ChainKey: key})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		auditRecords(t, st, n)
		h, err := st.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		h.Time = time.Now().UTC()
		if err := writeChainAnchor(cfg, h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	restored := commit(3)
	outDir := t.TempDir()
	var out bytes.Buffer
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", outDir}, &out)
	})
	replaced := commit(4)
	// 3, the backup's own record (TEN-398), then 4.
	if v, err := verifySecurityLog(ctx, c); err != nil || v.Break != nil || v.Records != 8 {
		t.Fatalf("before the restore: %+v, %v", v, err)
	}
	files, _ := filepath.Glob(filepath.Join(outDir, "tenant-backup-*.tar.gz"))
	if len(files) != 1 {
		t.Fatalf("backup files = %v", files)
	}
	out.Reset()
	runBackup(t, func() error {
		return cmdBackupRestore(ctx, []string{files[0], "--config", cfg, "--data", data, "--yes"}, strings.NewReader(""), &out)
	})
	if strings.Contains(out.String(), "Security log") {
		t.Errorf("restore warned about the Security log:\n%s", out.String())
	}
	// 3, the re-anchor, then the restore's own record.
	v, err := verifySecurityLog(ctx, c)
	if err != nil || v.Break != nil || v.Records != 5 || len(v.Restores) != 1 {
		t.Fatalf("after the restore: %+v, %v", v, err)
	}
	r := v.Restores[0]
	if r.Rec != 4 || r.Backup != filepath.Base(files[0]) || r.Restored.Rec != restored.Rec || r.Restored.Hash != restored.Hash ||
		r.Replaced.Rec != replaced.Rec || r.Replaced.Hash != replaced.Hash {
		t.Errorf("re-anchor record %+v; restored %+v, replaced %+v", r, restored, replaced)
	}
	if a := chainHeadIn(t, chainAnchorPath(cfg)); a == nil || a.Rec != 4 {
		t.Errorf("anchor after the restore: %+v", a)
	}

	// The audit export bundle verifies offline with the install's key.
	var bundle bytes.Buffer
	if err := exportSecurityLog(ctx, c, &bundle, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if hdr, v, err := verifySecurityBundle(c, &bundle); err != nil || v.Break != nil || v.Records != 5 || hdr.Anchor == nil || hdr.Anchor.Rec != 4 {
		t.Errorf("bundle %+v: %+v, %v", hdr, v, err)
	}
}
