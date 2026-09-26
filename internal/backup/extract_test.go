package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractWritesEveryEntryUnderItsRoot(t *testing.T) {
	m, open := stage(t)
	var buf bytes.Buffer
	if err := writeStream(&buf, m, open, nil); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	roots := map[string]string{RootConfig: filepath.Join(out, "cfg"), RootData: filepath.Join(out, "dat")}
	if err := extract(r, roots); err != nil {
		t.Fatal(err)
	}
	for _, f := range testFiles {
		root, rel, _ := strings.Cut(f.path, "/")
		got, err := os.ReadFile(filepath.Join(roots[root], filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != f.body {
			t.Fatalf("%s = %q, want %q", f.path, got, f.body)
		}
	}
}

func TestExtractStopsOnDamageAndNeverOverwrites(t *testing.T) {
	a := rawEntry{name: "data/a.json", body: "alpha"}
	b := rawEntry{name: "data/b.json", body: "bravo"}
	m := manifestFor(a, b)
	damaged := rawBackup(t, m, []rawEntry{a, {name: b.name, body: "brav0"}}, "")
	r, err := NewReader(bytes.NewReader(damaged), nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := extract(r, map[string]string{RootData: dir}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want a checksum failure", err)
	}

	// A second extract into the same staging dir must not overwrite.
	good := rawBackup(t, m, []rawEntry{a, b}, "")
	r, err = NewReader(bytes.NewReader(good), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := extract(r, map[string]string{RootData: dir}); err == nil {
		t.Fatal("extract overwrote an existing staged file")
	}
}

func TestExtractNeedsATargetForEveryRoot(t *testing.T) {
	m, open := stage(t)
	var buf bytes.Buffer
	if err := writeStream(&buf, m, open, nil); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := extract(r, map[string]string{RootData: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("err = %v, want a missing config target", err)
	}
}

func TestEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stage")
	if err := emptyDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := emptyDir(dir); err == nil {
		t.Fatal("emptyDir accepted a non-empty directory")
	}
}
