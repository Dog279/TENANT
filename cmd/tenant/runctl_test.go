package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/runtrace"
)

func TestCmdRunsExportWritesJSONL(t *testing.T) {
	ctx := context.Background()
	dataDir, cfgDir := t.TempDir(), t.TempDir()
	store, err := runtrace.Open(filepath.Join(dataDir, "runs.db"), runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := runtrace.Run{TraceID: "cli-trace", Source: "test", StartedAt: now, CompletedAt: &now, Outcome: "success"}
	if err := store.Start(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := cmdRuns(ctx, []string{"export", "--backend", "echo", "--data", dataDir, "--config", cfgDir, "--out", out}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got runtrace.Run
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("export is not one JSONL record: %v (%q)", err, b)
	}
	if got.TraceID != r.TraceID || got.Outcome != "success" {
		t.Fatalf("export = %+v", got)
	}
}
