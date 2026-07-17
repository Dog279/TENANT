package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadAndProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "token.json")
	if err := WriteFile(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Fatalf("ReadFile = %q, want secret", got)
	}
	report := Probe(path)
	if !report.Exists || !report.Protected || report.Mechanism == "" {
		t.Fatalf("unexpected protection report: %+v", report)
	}
}

func TestReadFileHardensExistingFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "token.json")
	if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if report := Probe(path); !report.Protected {
		t.Fatalf("legacy file was not hardened: %+v", report)
	}
}

func TestReadFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadFile(link); err == nil {
		t.Fatal("ReadFile accepted a symlink")
	}
}
