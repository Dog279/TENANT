package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"tenant/internal/fsperm"
)

// Settings is the persisted, per-agent runtime configuration — the state
// that must survive a restart. Today that's the tool enable/disable
// curation the operator does with /enable and /disable; the struct is a
// deliberate home for more knobs later (kept additive so old files load).
type Settings struct {
	// Tools maps a tool name to its persisted enabled state. It OVERRIDES
	// the launch-flag defaults on the next run, so the curated set the
	// operator left enabled is exactly what comes back. Tools missing from
	// the map keep their flag default (forward-compatible with new tools).
	Tools map[string]bool `json:"tools"`
	// Permissions maps a safety category (exec/destructive/web/send) to a
	// mode (ask/allow/deny). Persists "/approve always" and "/permissions
	// set" so the operator's safety choices survive restarts.
	Permissions map[string]string `json:"permissions,omitempty"`
	// KnownCategories lists the categories the operator has chosen a mode
	// for, saved with every permission change (TEN-343). One that asks and
	// isn't listed arrived with an update and is marked new. Absent beside
	// saved modes (a file from before the marker): only the original five
	// count as chosen.
	KnownCategories []string `json:"known_categories,omitempty"`
	// AnnouncedCategories are the new categories the terminal UI has already
	// told the operator about, so its launch notice shows once.
	AnnouncedCategories []string `json:"announced_categories,omitempty"`
	// ToolRules maps a tool pattern (an exact action id, a bare tool name, or
	// a glob) to a mode (ask/allow/deny) — the per-tool layer checked before
	// the category. Editable by hand: the running hub reloads this file when
	// it changes.
	ToolRules map[string]string `json:"tool_rules,omitempty"`
	// ExecProfiles holds the operator's per-surface execution-sandbox overrides,
	// keyed by surface or "default". Hand-edited; the running hub re-reads it.
	// There is deliberately no agent tool that writes it, and the file tools
	// refuse this file.
	ExecProfiles map[string]ExecProfile `json:"exec_profiles,omitempty"`
}

// ExecProfile is one operator override under "exec_profiles", keyed by a
// surface or "default" (every surface without its own entry). Unset fields keep
// the built-in default. deny_roots ADD to the built-in deny set; nothing can
// remove Tenant's own dirs from it.
type ExecProfile struct {
	Backend    string            `json:"backend,omitempty"`     // host | restricted
	Enforce    *bool             `json:"enforce,omitempty"`     // default true; false = dry-run
	Network    string            `json:"network,omitempty"`     // inherit | none | full (allowlist: rejected until enforced)
	Allow      []string          `json:"allow,omitempty"`       // reserved for a future enforced allowlist; rejected today
	ReadRoots  []string          `json:"read_roots,omitempty"`  // empty = whole host readable (deny roots still mask)
	WriteRoots []string          `json:"write_roots,omitempty"` // writable, in addition to the work dir
	DenyRoots  []string          `json:"deny_roots,omitempty"`  // added to the built-in deny set
	EnvPass    []string          `json:"env_pass,omitempty"`    // env allowlist: variable names to forward
	EnvSet     map[string]string `json:"env_set,omitempty"`     // env: fixed values
	CPUSeconds int               `json:"cpu_seconds,omitempty"`
	MemoryMB   int               `json:"memory_mb,omitempty"`
	MaxPIDs    int               `json:"max_pids,omitempty"`
}

// SettingsPath is the per-agent settings file, alongside the soul in the
// config dir.
func SettingsPath(cfgDir, agentID string) string {
	return filepath.Join(cfgDir, "settings."+agentID+".json")
}

// LoadSettings reads the per-agent settings. A missing file is NOT an
// error — it's first run, so we return empty settings. A present-but-
// corrupt file returns the error so the caller can warn (and we fall back
// to defaults rather than wiping the user's curation silently).
func LoadSettings(cfgDir, agentID string) (*Settings, error) {
	s := &Settings{Tools: map[string]bool{}}
	b, err := os.ReadFile(SettingsPath(cfgDir, agentID))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return &Settings{Tools: map[string]bool{}}, fmt.Errorf("settings file corrupt: %w", err)
	}
	if s.Tools == nil {
		s.Tools = map[string]bool{}
	}
	return s, nil
}

// Save writes the settings atomically (temp + rename) so a crash mid-write
// can't leave a truncated file that fails to parse next launch, and files the
// change in the Security log.
func (s *Settings) Save(cfgDir, agentID string) error {
	if s.Tools == nil {
		s.Tools = map[string]bool{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := SettingsPath(cfgDir, agentID)
	if err := fsperm.MkdirAll(filepath.Dir(p)); err != nil {
		return err
	}
	return saveAudited(p, b)
}
