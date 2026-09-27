package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// TEN-375: a stored credential that reached the log book is found, in a text
// file (raw and base64-encoded) and in logbook.db, and reported by name and
// place: the audit never prints the value, as JSON or as text.
func TestSecurityAuditFindsKnownSecretsInTheLogbook(t *testing.T) {
	const secret = "planted-value-4f9c2e71ab"
	cfg, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(credentialsPath(cfg), []byte(`{"secrets":{"p1":"`+secret+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(data, "logs")
	mirror := filepath.Join(root, "tools", "2026-09-27.log")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := "2026-09-27 10:00:00.000 INFO  a clean line\n" +
		"2026-09-27 10:00:01.000 INFO  curl -H 'X-Key: " + secret + "'\n" +
		"2026-09-27 10:00:02.000 INFO  basic " + base64.StdEncoding.EncodeToString([]byte("user:"+secret)) + "\n"
	if err := os.WriteFile(mirror, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	batch := []event.Event{
		{Log: "tools", Provider: "tools", Level: event.Info, Msg: "clean"},
		{Log: "tools", Provider: "tools", Level: event.Info, Msg: "ran a tool", Data: []event.Field{{Key: "args", Value: `{"key":"` + secret + `"}`}}},
	}
	if err := st.Commit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out, _ := captureTenantStdout(t, func() error {
		return cmdSecurity(context.Background(), []string{"audit", "--config", cfg, "--data", data, "--json"})
	})
	text, _ := captureTenantStdout(t, func() error {
		return cmdSecurity(context.Background(), []string{"audit", "--config", cfg, "--data", data})
	})
	for _, form := range []string{secret, base64.StdEncoding.EncodeToString([]byte("user:" + secret))} {
		if strings.Contains(out, form) || strings.Contains(text, form) {
			t.Fatalf("the audit printed the secret (%q)", form)
		}
	}
	report, _ := runSecurityAuditJSON(t, "--config", cfg, "--data", data)
	lb := report.Logbook
	const name = "credentials.json:secrets.p1"
	want := []secretHit{
		{Secret: name, Where: "logbook.db", Count: 1, At: []int64{int64(batch[1].Seq)}},
		{Secret: name, Where: "tools/2026-09-27.log", Count: 2, At: []int64{2, 3}},
	}
	if !slices.EqualFunc(lb.Scan.Hits, want, func(a, b secretHit) bool {
		return a.Secret == b.Secret && a.Where == b.Where && a.Count == b.Count && slices.Equal(a.At, b.At)
	}) {
		t.Errorf("hits = %+v, want %+v", lb.Scan.Hits, want)
	}
	if lb.Scan.MirrorFiles != 1 || lb.Scan.StoreEvents != 2 || !lb.Scrubber.On || lb.Scrubber.KnownValues < 1 {
		t.Errorf("scan %+v, scrubber %+v", lb.Scan, lb.Scrubber)
	}
	if !hasSecurityFinding(report.Findings, "logbook.secrets") {
		t.Errorf("no logbook.secrets finding: %+v", report.Findings)
	}
	if !strings.Contains(text, "tools/2026-09-27.log: "+name+" × 2 (line [2 3])") {
		t.Errorf("the text report lacks the hit:\n%s", text)
	}
	if !slices.ContainsFunc(report.Files, func(f fileProtectionAudit) bool { return f.Path == filepath.Join(root, "logbook.db") }) ||
		!slices.ContainsFunc(report.Files, func(f fileProtectionAudit) bool { return f.Path == filepath.Join(root, "tools") }) {
		t.Errorf("file_protection lacks the log book: %+v", report.Files)
	}
}

// TEN-375: the audit verifies the Security log and records when; an edited
// record is a broken chain, a critical finding. The witness is reported.
func TestSecurityAuditVerifiesTheSecurityLog(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	witness := filepath.Join(t.TempDir(), "witness.json")
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"logs":{"security":{"witness":"`+filepath.ToSlash(witness)+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runSecurityAuditJSON(t, "--config", cfg, "--data", data)
	if v := report.Logbook.Verify; v.Result != "no_store" || err != nil {
		t.Fatalf("a fresh install: %+v, %v", v, err)
	}
	if w := report.Logbook.Witness; !w.Configured || w.Kind != "file" || w.Target != witness || w.Problem != "" {
		t.Errorf("witness = %+v", w)
	}

	key, err := chainKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "logs", "logbook.db")
	st, err := store.Open(path, store.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	auditRecords(t, st, 3)
	st.Close()
	report, err = runSecurityAuditJSON(t, "--config", cfg, "--data", data)
	if v := report.Logbook.Verify; v.Result != "ok" || v.Records != 3 || v.CheckedAt.IsZero() || err != nil {
		t.Fatalf("an intact chain: %+v, %v", v, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE events SET msg = 'forged' WHERE rec = 2`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	report, err = runSecurityAuditJSON(t, "--config", cfg, "--data", data)
	if v := report.Logbook.Verify; v.Result != "broken" || !strings.Contains(v.Break, "record 2") || v.CheckedAt.IsZero() {
		t.Errorf("an edited record: %+v", v)
	}
	if !hasSecurityFinding(report.Findings, "logbook.chain") || report.Critical == 0 || err == nil {
		t.Errorf("a broken chain is not a critical finding: %+v, %v", report.Findings, err)
	}
}
