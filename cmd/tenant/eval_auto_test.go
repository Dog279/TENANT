package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestAutoEnableEvalPlugins_AllOffBaseline(t *testing.T) {
	pf := &pluginFlags{}
	c := &commonFlags{dataDir: t.TempDir(), cfgDir: t.TempDir()}
	explicit := map[string]bool{}

	enabled := autoEnableEvalPlugins(pf, c, explicit)

	if !pf.web {
		t.Error("pf.web should be auto-enabled")
	}
	if !pf.osEnable {
		t.Error("pf.osEnable should be auto-enabled")
	}
	if !pf.osAllowExec {
		t.Error("pf.osAllowExec should be set when os is auto-enabled")
	}
	if !pf.osAllowWrite {
		t.Error("pf.osAllowWrite should be set when os is auto-enabled")
	}

	// Must contain at least "web" and "os".
	has := map[string]bool{}
	for _, name := range enabled {
		has[name] = true
	}
	if !has["web"] {
		t.Error(`enabled should contain "web"`)
	}
	if !has["os"] {
		t.Error(`enabled should contain "os"`)
	}
}

func TestAutoEnableEvalPlugins_ExplicitOverrideRespected(t *testing.T) {
	// Simulate: operator passed --web explicitly (fs.Visit recorded it),
	// but pf.web is still false (they didn't pass --web, or their own
	// logic left it off). The auto-enabler must NOT flip pf.web to true.
	pf := &pluginFlags{}
	c := &commonFlags{dataDir: t.TempDir(), cfgDir: t.TempDir()}
	explicit := map[string]bool{"web": true}

	enabled := autoEnableEvalPlugins(pf, c, explicit)

	if pf.web {
		t.Error("pf.web should NOT be flipped when explicitFlags[\"web\"] is true")
	}
	for _, name := range enabled {
		if name == "web" {
			t.Error(`"web" must not appear in enabled when explicitly overridden`)
		}
	}
}

func TestAutoEnableEvalPlugins_SQLAutoDetect(t *testing.T) {
	// Create a temp dir with tenant.db so findFirstExisting finds it.
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "tenant.db")
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	pf := &pluginFlags{}
	c := &commonFlags{dataDir: dataDir, cfgDir: t.TempDir()}
	explicit := map[string]bool{}

	enabled := autoEnableEvalPlugins(pf, c, explicit)

	hasSQL := false
	for _, name := range enabled {
		if name == "sql" {
			hasSQL = true
		}
	}
	if !hasSQL {
		t.Error("sql should be auto-detected when tenant.db exists in dataDir")
	}
	if pf.sqlDB != dbPath {
		t.Errorf("pf.sqlDB = %q, want %q", pf.sqlDB, dbPath)
	}
	if !pf.sqlAllowWrite {
		t.Error("pf.sqlAllowWrite should be set when sql is auto-detected")
	}
}

func TestAutoEnableEvalPlugins_WikiFromLaunchConfig(t *testing.T) {
	// Create a wiki directory and a config.json with wiki enabled.
	cfgDir := t.TempDir()
	wikiDir := t.TempDir()

	cfg := &launchConfig{
		Skills: map[string]*skillConfig{
			"wiki": {
				Enabled: true,
				Settings: map[string]string{
					"dir": wikiDir,
				},
			},
		},
	}
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	pf := &pluginFlags{}
	c := &commonFlags{dataDir: t.TempDir(), cfgDir: cfgDir}
	explicit := map[string]bool{}

	enabled := autoEnableEvalPlugins(pf, c, explicit)

	hasWiki := false
	for _, name := range enabled {
		if name == "wiki" {
			hasWiki = true
		}
	}
	if !hasWiki {
		enabledSorted := make([]string, len(enabled))
		copy(enabledSorted, enabled)
		sort.Strings(enabledSorted)
		t.Errorf("wiki should be auto-enabled from launchConfig; enabled = %v", enabledSorted)
	}
	if pf.wikiDir != wikiDir {
		t.Errorf("pf.wikiDir = %q, want %q", pf.wikiDir, wikiDir)
	}
}

func TestAutoEnableEvalPlugins_ExplicitWikiDirNotOverridden(t *testing.T) {
	// If the operator explicitly passed --wiki-dir, the auto-enabler must
	// not change it.
	cfgDir := t.TempDir()
	wikiDir := t.TempDir()

	cfg := &launchConfig{
		Skills: map[string]*skillConfig{
			"wiki": {
				Enabled: true,
				Settings: map[string]string{
					"dir": wikiDir,
				},
			},
		},
	}
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	pf := &pluginFlags{}
	c := &commonFlags{dataDir: t.TempDir(), cfgDir: cfgDir}
	explicit := map[string]bool{"wiki-dir": true}

	enabled := autoEnableEvalPlugins(pf, c, explicit)

	for _, name := range enabled {
		if name == "wiki" {
			t.Error("wiki should NOT be auto-enabled when explicitFlags[\"wiki-dir\"] is true")
		}
	}
}

func TestAutoEnableEvalPlugins_ExplicitSQLDBNotOverridden(t *testing.T) {
	// If the operator explicitly passed --sql-db, auto-detect must not
	// change it — even when a tenant.db exists.
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "tenant.db")
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	pf := &pluginFlags{}
	c := &commonFlags{dataDir: dataDir, cfgDir: t.TempDir()}
	explicit := map[string]bool{"sql-db": true}

	enabled := autoEnableEvalPlugins(pf, c, explicit)

	for _, name := range enabled {
		if name == "sql" {
			t.Error("sql should NOT be auto-enabled when explicitFlags[\"sql-db\"] is true")
		}
	}
}

func TestAutoEnableEvalPlugins_NoDBFile_NoSQL(t *testing.T) {
	// Empty dataDir → no tenant.db → sql should NOT be auto-enabled.
	pf := &pluginFlags{}
	c := &commonFlags{dataDir: t.TempDir(), cfgDir: t.TempDir()}
	explicit := map[string]bool{}

	enabled := autoEnableEvalPlugins(pf, c, explicit)

	for _, name := range enabled {
		if name == "sql" {
			t.Error("sql should NOT be auto-enabled when no DB file exists")
		}
	}
}
