//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/improve"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
	"tenant/internal/plugins/mcpremote"
)

// TEN-333: plain `tenant doctor` (no --fix) is read-only. It has to report
// loose permissions, not quietly tighten files before its own audit runs.
func TestDoctorWithoutFixLeavesModesAlone(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	seedLooseTenantTree(t, cfg, data)
	before := modeSnapshot(t, cfg, data)

	report := runDoctorJSON(t, "--config", cfg, "--data", data)

	after := modeSnapshot(t, cfg, data)
	for path, mode := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("plain doctor removed %s", path)
			continue
		}
		if got != mode {
			t.Errorf("plain doctor changed %s from %04o to %04o", path, mode, got)
		}
	}
	perm := report["private data permissions"]
	if perm.Stat != "WARN" || !strings.Contains(perm.Detail, "exceed owner-only") {
		t.Errorf("private data permissions = %+v, want a WARN reporting the loose paths", perm)
	}
}

// TEN-333 counterpart: --fix is where the tightening happens.
func TestDoctorFixTightensLooseModes(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	seedLooseTenantTree(t, cfg, data)

	runDoctorJSON(t, "--config", cfg, "--data", data, "--fix")

	for path, mode := range modeSnapshot(t, cfg, data) {
		want := fs.FileMode(0o600)
		if info, err := os.Lstat(path); err == nil && info.IsDir() {
			want = 0o700
		}
		if mode&^want != 0 {
			t.Errorf("doctor --fix left %s at %04o, want at most %04o", path, mode, want)
		}
	}
}

// seedLooseTenantTree builds config and data roots the way an older Tenant
// or a 022 umask leaves them: directories 0755, files 0644.
func seedLooseTenantTree(t *testing.T, cfg, data string) {
	t.Helper()
	for name, open := range map[string]func(string) error{
		"episodes.db": func(p string) error { s, err := episodic.Open(p); closeIfOpen(s, err); return err },
		"facts.db":    func(p string) error { s, err := semantic.Open(p); closeIfOpen(s, err); return err },
		"skills.db":   func(p string) error { s, err := skills.Open(p); closeIfOpen(s, err); return err },
		"tenant_meta.db": func(p string) error {
			m, err := improve.OpenMeta(p)
			if err == nil {
				_ = m.Close()
			}
			return err
		},
	} {
		if err := open(filepath.Join(data, name)); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// The config reaches every credential reader doctor has: a stored
	// provider key (read while resolving flags), an agent profile on it, X and
	// Discord (their checks resolve stored tokens), and a remote MCP connector
	// whose token cache the offline health check reads. Endpoints are closed
	// local ports, so no check waits on the network.
	const mcpURL = "https://mcp.example.test/sse"
	files := map[string]string{
		filepath.Join(cfg, "config.json"): `{
			"provider": "p1",
			"providers": {"p1": {"kind": "openai", "endpoint": "http://127.0.0.1:9", "model": "m", "auth": {"stored": true}}},
			"agents": {"researcher": {"provider": "p1"}},
			"skills": {"x": {"enabled": true}, "discord": {"enabled": true}},
			"mcp_remotes": ["` + mcpURL + `"]
		}`,
		filepath.Join(cfg, "credentials.json"):                      `{"secrets":{"p1":"sk-test"}}`,
		mcpremote.TokenCachePath(filepath.Join(cfg, "mcp"), mcpURL): `{}`,
		filepath.Join(data, "x-token.json"):                         `{}`,
		filepath.Join(data, "archive", "2026-09", "s.jsonl"):        "{}\n",
		filepath.Join(data, "research", "notes.md"):                 "notes\n",
		filepath.Join(data, "eval-artifacts", "report-smoke.md"):    "report\n",
		filepath.Join(data, "eval-artifacts", "trend-smoke.json"):   "{}\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range []string{cfg, data} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if d.IsDir() {
				mode = 0o755
			}
			return os.Chmod(path, mode)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type closer interface{ Close() error }

func closeIfOpen(c closer, err error) {
	if err == nil {
		_ = c.Close()
	}
}

// modeSnapshot records the permission bits of every path under the roots.
func modeSnapshot(t *testing.T, roots ...string) map[string]fs.FileMode {
	t.Helper()
	modes := map[string]fs.FileMode{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			modes[path] = info.Mode().Perm()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return modes
}

// runDoctorJSON runs `tenant doctor --json` with the given flags and returns
// its checks by name. A FAILing check is not an error here: these tests are
// about what doctor changes, not whether the seeded install is healthy.
func runDoctorJSON(t *testing.T, args ...string) map[string]checkResult {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "doctor-*.json")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = out
	_ = cmdDoctor(context.Background(), append(args, "--json"))
	os.Stdout = stdout
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Checks []checkResult `json:"checks"`
	}
	if err := json.NewDecoder(out).Decode(&report); err != nil {
		t.Fatalf("decode doctor --json output: %v", err)
	}
	_ = out.Close()
	byName := map[string]checkResult{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	return byName
}
