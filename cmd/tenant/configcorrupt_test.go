package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A corrupt config.json stops a command before anything can save the flag
// defaults over it, and says where the file broke and how to recover.
func TestCorruptConfigRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	broken := "{\n  \"providers\": {,\n}\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &commonFlags{cfgDir: dir, dataDir: t.TempDir()}
	err := c.applyLaunchConfig()
	if err == nil || !strings.Contains(err.Error(), "at line 2, column 17") || !strings.Contains(err.Error(), "tenant setup") {
		t.Fatalf("err = %v, want the position and the way out", err)
	}
	if c.lc != nil {
		t.Fatalf("a corrupt config left a config to save: %+v", c.lc)
	}
	if got, _ := os.ReadFile(path); string(got) != broken {
		t.Fatalf("config.json changed: %q", got)
	}
}
