package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/memory/soul"
)

// expandPath expands a leading ~ (or ~/, ~\) to the user's home dir.
// Go doesn't do this and neither does PowerShell, so a literal
// "~/notes" would otherwise be treated as a relative dir named "~".
func expandPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// defaultSQLDBPath is the SQLite file the sql plugin falls back to when no
// path is given: ~/Desktop/tenant.db. Kept on the Desktop so it's easy to
// find and inspect during the single-operator workstation stage. The file
// (and ~/Desktop if somehow absent) is created on first open by sql.Open.
func defaultSQLDBPath() string {
	return expandPath(filepath.Join("~", "Desktop", "tenant.db"))
}

// ensureSetup is the idempotent first-run installer: it detects what
// exists and creates only what's missing — data/config dirs, an
// editable default soul (identity) file, the wiki vault (+ a starter
// note), and the sql db's parent dir — reporting each action so the
// caller can show "what got set up". Safe to run on every launch.
func ensureSetup(c *commonFlags, pf *pluginFlags) []string {
	var lines []string
	ensureDir := func(label, path string) {
		if path == "" {
			return
		}
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			lines = append(lines, "found "+label+": "+path)
			return
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			lines = append(lines, "FAILED "+label+": "+err.Error())
			return
		}
		lines = append(lines, "created "+label+": "+path)
	}

	ensureDir("data dir", c.dataDir)
	ensureDir("config dir", c.cfgDir)

	// Default soul/identity — first run seeds an editable TOML file so
	// the operator can shape the agent's persona later.
	soulPath := soul.Path(c.cfgDir, c.agent)
	if _, err := os.Stat(soulPath); err == nil {
		lines = append(lines, "found soul: "+soulPath)
	} else if serr := soul.NewDefault(c.agent).Save(c.cfgDir); serr != nil {
		lines = append(lines, "FAILED soul: "+serr.Error())
	} else {
		lines = append(lines, "created soul: "+soulPath)
	}

	if pf.wikiDir != "" {
		ensureDir("wiki dir", pf.wikiDir)
		seedWikiNote(pf.wikiDir, &lines)
	}
	if pf.sqlDB != "" {
		ensureDir("sql dir", filepath.Dir(pf.sqlDB))
	}
	return lines
}

// seedWikiNote drops a starter note into a wiki vault that has none, so
// a freshly-created vault isn't empty/confusing. No-op if any note
// already exists (idempotent).
func seedWikiNote(dir string, lines *[]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown") || strings.HasSuffix(n, ".txt") {
			return // already has notes
		}
	}
	p := filepath.Join(dir, "welcome.md")
	content := "# Welcome to your Tenant wiki\n\n" +
		"Drop markdown notes in this folder. The agent can search them with " +
		"wiki_search and follow [[wikilinks]] between them.\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err == nil {
		*lines = append(*lines, "seeded starter note: "+p)
	}
}

// healthCheck probes the configured endpoints (best-effort, short
// timeout) so the operator sees connectivity at a glance on launch —
// the "noted debug". Never blocks startup meaningfully.
func healthCheck(ctx context.Context, c *commonFlags) []string {
	if c.backend == "echo" {
		return []string{"backend: echo (offline dev — responses are deterministic stubs)"}
	}
	var lines []string
	if c.vllmEndpoint != "" {
		if reachable(ctx, c.vllmEndpoint+"/v1/models") {
			lines = append(lines, "generation OK: "+c.vllmModel+" @ "+c.vllmEndpoint)
		} else {
			lines = append(lines, "GENERATION UNREACHABLE: "+c.vllmEndpoint+" — chat will fail")
		}
	}
	switch {
	case c.embedEndpoint == "":
		lines = append(lines, "embeddings: in-process hash (no --embed-endpoint) — semantic recall degraded")
	case reachable(ctx, c.embedEndpoint+"/v1/models"):
		lines = append(lines, "embeddings OK: "+c.embedModel+" @ "+c.embedEndpoint)
	default:
		lines = append(lines, "EMBEDDINGS UNREACHABLE: "+c.embedEndpoint+" — memory/wiki search degraded")
	}
	return lines
}

func reachable(ctx context.Context, url string) bool {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}
