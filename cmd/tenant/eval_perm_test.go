//go:build !windows

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/eval"
)

// TEN-333: eval artifacts live in the private data dir. Each writer creates
// its files owner-only and tightens a loose existing destination, so a run
// after `doctor --fix` does not widen the tree again under a 022 umask.

func TestEvalReportWriterIsPrivate(t *testing.T) {
	dir := looseArtifactDir(t)
	artifact := writeEvalArtifact(context.Background(), dir, smokeReport())
	if artifact == "" {
		t.Fatal("writeEvalArtifact failed")
	}
	assertPermIs(t, dir, 0o700)
	assertPermIs(t, artifact, 0o600)
}

func TestEvalTrendWriterTightensExistingFile(t *testing.T) {
	dir := looseArtifactDir(t)
	trend := evalTrendPath(dir)
	writeLoose(t, trend)
	appendEvalTrend(context.Background(), dir, trendEntryFor(context.Background(), smokeReport(), "", ""))
	assertPermIs(t, dir, 0o700)
	assertPermIs(t, trend, 0o600)
	if entries, err := readEvalTrend(dir); err != nil || len(entries) != 1 {
		t.Fatalf("trend entries = %v, %v; want the appended line", entries, err)
	}
}

func TestBaselineFromArtifactIsPrivate(t *testing.T) {
	src := t.TempDir()
	artifact := writeEvalArtifact(context.Background(), src, smokeReport())
	if artifact == "" {
		t.Fatal("writeEvalArtifact failed")
	}
	dir := looseArtifactDir(t)
	baseline := filepath.Join(dir, "baseline.smoke.json")
	writeLoose(t, baseline)
	if err := baselineFromArtifact(artifact, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertPermIs(t, dir, 0o700)
	assertPermIs(t, baseline, 0o600)
}

func smokeReport() *eval.Report {
	return &eval.Report{Subset: eval.SubsetSmoke, Aggregates: eval.Aggregates{Overall: 100, PassCount: 1}}
}

// looseArtifactDir is an existing eval-artifacts dir left 0755 by an older
// build or a 022 umask.
func looseArtifactDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "eval-artifacts")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeLoose(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertPermIs(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}
