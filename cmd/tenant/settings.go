package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// settings is the persisted, per-agent runtime configuration — the state
// that must survive a restart. Today that's the tool enable/disable
// curation the operator does with /enable and /disable; the struct is a
// deliberate home for more knobs later (kept additive so old files load).
type settings struct {
	// Tools maps a tool name to its persisted enabled state. It OVERRIDES
	// the launch-flag defaults on the next run, so the curated set the
	// operator left enabled is exactly what comes back. Tools missing from
	// the map keep their flag default (forward-compatible with new tools).
	Tools map[string]bool `json:"tools"`
	// Permissions maps a safety category (exec/destructive/web/send) to a
	// mode (ask/allow/deny). Persists "/approve always" and "/permissions
	// set" so the operator's safety choices survive restarts.
	Permissions map[string]string `json:"permissions,omitempty"`
}

// settingsPath is the per-agent settings file, alongside the soul in the
// config dir.
func settingsPath(cfgDir, agentID string) string {
	return filepath.Join(cfgDir, "settings."+agentID+".json")
}

// loadSettings reads the per-agent settings. A missing file is NOT an
// error — it's first run, so we return empty settings. A present-but-
// corrupt file returns the error so the caller can warn (and we fall back
// to defaults rather than wiping the user's curation silently).
func loadSettings(cfgDir, agentID string) (*settings, error) {
	s := &settings{Tools: map[string]bool{}}
	b, err := os.ReadFile(settingsPath(cfgDir, agentID))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return &settings{Tools: map[string]bool{}}, fmt.Errorf("settings file corrupt: %w", err)
	}
	if s.Tools == nil {
		s.Tools = map[string]bool{}
	}
	return s, nil
}

// save writes the settings atomically (temp + rename) so a crash mid-write
// can't leave a truncated file that fails to parse next launch.
func (s *settings) save(cfgDir, agentID string) error {
	if s.Tools == nil {
		s.Tools = map[string]bool{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := settingsPath(cfgDir, agentID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
