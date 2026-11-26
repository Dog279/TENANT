package fsperm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A reader that has the file open for a moment (ours, a scanner) does not
// fail a save: the rename waits for it.
func TestReplace_WaitsForAReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := Replace(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(path) // no FILE_SHARE_DELETE: a rename over it is refused
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(60 * time.Millisecond); r.Close() }()
	if err := Replace(path, []byte("new")); err != nil {
		t.Fatalf("a save while a reader held the file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("the file holds %q", got)
	}
}
