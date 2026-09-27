package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyLogCutsATornTail(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sess.jsonl")
	body := "{\"n\":1}\n{\"n\":2}\n{\"n\":3, \"partial"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.jsonl")
	if err := copyLog(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"n\":1}\n{\"n\":2}\n" {
		t.Fatalf("copy = %q", got)
	}
	if err := checkLog(dst); err != nil {
		t.Fatal(err)
	}
}

func TestCopyLogLongFileAndNoNewline(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.jsonl")
	line := "{\"pad\":\"" + strings.Repeat("x", 1000) + "\"}\n"
	body := strings.Repeat(line, 200) + "{\"torn\":"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.jsonl")
	if err := copyLog(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(line)*200) {
		t.Fatalf("copied %d bytes, want %d", info.Size(), len(line)*200)
	}
	only := filepath.Join(dir, "torn.jsonl")
	if err := os.WriteFile(only, []byte("{\"never finished"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.jsonl")
	if err := copyLog(only, empty); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(empty); info.Size() != 0 {
		t.Fatalf("a log with no complete record copied %d bytes", info.Size())
	}
}

func TestCheckLogReportsTheBadLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte("{\"ok\":1}\n\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkLog(path)
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("err = %v, want line 3 named", err)
	}
}
