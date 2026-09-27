package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
	"tenant/internal/memory/episodic"
	"tenant/internal/runtrace"
)

func logbookMsgs(t *testing.T, path string) (id [16]byte, msgs []string) {
	t.Helper()
	st, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, _, _, err := st.Since(context.Background(), store.Cursor{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		msgs = append(msgs, e.Msg)
	}
	return st.StoreID(), msgs
}

// A backup taken while this process has logbook.db open captures it without
// disturbing the writer, verifies it as the store it is, leaves the text
// mirror out, and restores it with its store ID.
func TestBackupCarriesTheOpenLogbook(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	path := filepath.Join(data, "logs", "logbook.db")
	st, err := store.Open(path, store.Options{BootID: runtrace.BootID()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commit := func(msg string) error {
		return st.Commit(ctx, []event.Event{{Log: "turns", Provider: "turns", Level: event.Info, Msg: msg}})
	}
	if err := commit("before the backup"); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(data, "logs", "turns", "2026-09-27.log")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mirror, []byte("2026-09-27 10:00:00.000 INFO  [turns] before the backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	var out bytes.Buffer
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", outDir}, &out)
	})
	if !strings.Contains(out.String(), "text mirror") {
		t.Errorf("create does not say the mirror was left out:\n%s", out.String())
	}
	if err := commit("after the backup"); err != nil {
		t.Fatalf("commit after the backup: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(outDir, "tenant-backup-*.tar.gz"))
	if len(files) != 1 {
		t.Fatalf("backup files = %v", files)
	}
	out.Reset()
	runBackup(t, func() error { return cmdBackupVerify(ctx, []string{files[0]}, &out) })
	if !strings.Contains(out.String(), "OK:") || strings.Contains(out.String(), "not a store this Tenant knows") {
		t.Fatalf("verify:\n%s", out.String())
	}
	id := st.StoreID()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	runBackup(t, func() error {
		return cmdBackupRestore(ctx, []string{files[0], "--config", cfg, "--data", data, "--yes"}, strings.NewReader(""), &out)
	})
	// The events come back, and the Security log's re-anchor record joins
	// them (TEN-396), then the restore's own events, its Security record
	// last (TEN-398); the store ID does not, so live-tail cursors from before
	// the restore start over instead of skipping new rows.
	if got, msgs := logbookMsgs(t, path); got == id || len(msgs) < 3 || msgs[0] != "before the backup" || !strings.HasPrefix(msgs[1], "The Security log was restored") ||
		!strings.HasSuffix(msgs[len(msgs)-1], "restored over this install") {
		t.Errorf("restored log book: store ID %x (was %x, want a new one), events %q", got, id, msgs)
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Errorf("the text mirror came back from a backup that left it out: %v", err)
	}
}

// With the log book moved by logs.dir, forgetting a session clears it there,
// and status then counts the backup made before the forget.
func TestForgetReachesARelocatedLogbookAndStatusCountsBackups(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	logsDir := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2,"logs":{"dir":"`+filepath.ToSlash(logsDir)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ep, err := episodic.Open(filepath.Join(data, "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ep.Insert(ctx, &episodic.Episode{AgentID: "main", SessionID: "s1", Prompt: "my plate is XYZ-123", Response: "ok", EmbedderID: "echo/4", Embedding: []float32{0.1, 0.2, 0.3, 0.4}}); err != nil {
		t.Fatal(err)
	}
	ep.Close()
	path := filepath.Join(logsDir, "logbook.db")
	st, err := store.Open(path, store.Options{ChainKey: []byte("test chain key")})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(ctx, []event.Event{
		{Log: "turns", Provider: "turns", Level: event.Info, Msg: "plate XYZ-123", Corr: event.Correlation{SessionID: "s1"}},
		{Log: "security", Provider: "security", Level: event.Info, Msg: "approval for s1", Corr: event.Correlation{SessionID: "s1"}},
		{Log: "turns", Provider: "turns", Level: event.Info, Msg: "unrelated"},
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var out bytes.Buffer
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", t.TempDir()}, &out)
	})
	if !strings.Contains(out.String(), logsDir+": log book, outside Tenant's directories") {
		t.Errorf("create does not list the moved log book as not included:\n%s", out.String())
	}
	out.Reset()
	if err := cmdPrivacyForget(ctx, privacyArgs(cfg, data, "--session", "s1", "--yes"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "log book events in turns") || !strings.Contains(out.String(), "log book security records") {
		t.Errorf("forget output does not list the log book:\n%s", out.String())
	}
	// The backup's and the forget's own events follow, the forget's Security
	// record last (TEN-398).
	if _, msgs := logbookMsgs(t, path); len(msgs) < 3 || !slices.Equal(msgs[:2], []string{store.Tombstone, "unrelated"}) ||
		!strings.HasPrefix(msgs[len(msgs)-1], "Privacy deletion (forget) by the operator: delete 1 episodes") {
		t.Errorf("relocated log book after the forget: %q", msgs)
	}
	out.Reset()
	if err := cmdPrivacyStatus(ctx, privacyArgs(cfg, data), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ": 1 backup(s) made before it still hold what it removed") {
		t.Errorf("status does not count the backup made before the forget:\n%s", out.String())
	}
}
