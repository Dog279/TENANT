package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~":         home,
		"~/notes":   filepath.Join(home, "notes"),
		"/abs/path": "/abs/path",
		"rel/dir":   "rel/dir",
		"":          "",
		"~tricky":   "~tricky", // only a leading ~ or ~/ expands
	}
	for in, want := range cases {
		if got := expandPath(in); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ensureSetup creates only what's missing and is idempotent.
func TestEnsureSetup_CreatesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	c := &commonFlags{agent: "main", dataDir: filepath.Join(root, "data"), cfgDir: filepath.Join(root, "cfg")}
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil { // resolve() normally does this
		t.Fatal(err)
	}
	pf := &pluginFlags{wikiDir: filepath.Join(root, "notes"), sqlDB: filepath.Join(root, "db", "app.db")}

	first := ensureSetup(c, pf)
	// Everything should now exist.
	for _, p := range []string{c.cfgDir, pf.wikiDir, filepath.Join(pf.wikiDir, "welcome.md"), filepath.Dir(pf.sqlDB)} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist after setup: %v", p, err)
		}
	}
	if len(first) == 0 {
		t.Fatal("expected setup report lines")
	}
	// Second run: idempotent — nothing reported as "created".
	for _, ln := range ensureSetup(c, pf) {
		if len(ln) >= 7 && ln[:7] == "created" {
			t.Errorf("second run should not create anything, got: %q", ln)
		}
	}
}
