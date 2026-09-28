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

func TestProtectFileLeavesParentAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backup.tar.gz")
	if err := os.WriteFile(path, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirOKBefore, _, dirBefore, err := probePath(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProtectFile(path); err != nil {
		t.Fatal(err)
	}
	if ok, _, detail, err := probePath(path, false); err != nil || !ok {
		t.Fatalf("file not protected: ok=%v detail=%q err=%v", ok, detail, err)
	}
	dirOKAfter, _, dirAfter, err := probePath(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if dirOKBefore != dirOKAfter || dirBefore != dirAfter {
		t.Fatalf("parent protection changed: %v %q -> %v %q", dirOKBefore, dirBefore, dirOKAfter, dirAfter)
	}
}

func TestProtectFileRejectsSymlinkAndDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := ProtectFile(dir); err == nil {
		t.Fatal("ProtectFile accepted a directory")
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ProtectFile(link); err == nil {
		t.Fatal("ProtectFile accepted a symlink")
	}
}

// The OnWrite hook sees every secret this process writes or creates, and not
// a create that lost the race.
func TestOnWriteSeesWritesAndCreates(t *testing.T) {
	var got []string
	OnWrite(func(path string) { got = append(got, filepath.Base(path)) })
	t.Cleanup(func() { OnWrite(nil) })
	dir := t.TempDir()
	if err := WriteFile(filepath.Join(dir, "a.json"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := CreateExclusive(filepath.Join(dir, "b"), []byte("2")); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0] != "a.json" || got[1] != "b" {
		t.Fatalf("hook saw %v, want [a.json b]", got)
	}
}
