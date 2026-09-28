package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config.json this account can't read stops the command: starting on the
// flag defaults instead would bring serve up on the echo backend with no relay
// and no cron, looking healthy while ignoring the operator's config.
func TestResolve_RefusesUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	makeUnreadable(t, path)
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this account reads the file anyway")
	}

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse([]string{"--config", dir, "--data", filepath.Join(dir, "data")}); err != nil {
		t.Fatal(err)
	}
	err := c.resolve()
	if err == nil || !strings.Contains(err.Error(), "config.json is unreadable") {
		t.Fatalf("resolve = %v, want the unreadable config.json refused", err)
	}
}
