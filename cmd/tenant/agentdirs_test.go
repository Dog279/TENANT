package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// `tenant agents` edits the config its --config names, in either flag form
// and anywhere among the arguments, and never reads a flag as the model.
func TestAgentsCommandHonorsTheDirFlags(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	cfg := `{"schema_version":2,"provider":"zai","providers":{"zai":{"kind":"zai","model":"glm-4.6"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdAgents(context.Background(), []string{"add", "writer", "zai", "--config", dir, "--data=" + data}); err != nil {
		t.Fatal(err)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := lc.Agents["writer"]
	if a == nil || a.Provider != "zai" || a.Model != "" {
		t.Fatalf("agent in --config = %+v, want provider zai, inheriting its model", a)
	}
	if _, _, err := agentDirs([]string{"list", "--config"}); err == nil {
		t.Fatal("a --config with no directory was accepted")
	}
}
