package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	// ExecExceptions are folders a surface's sandboxed commands may write to,
	// keyed by surface: added to its write roots whichever profile it runs
	// under. Approving a sandbox escalation "always" adds the refused folder;
	// the operator removes one by deleting it. Like exec_profiles, no agent
	// tool writes it and the running hub re-reads it.
	ExecExceptions map[string][]string `json:"exec_exceptions,omitempty"`
	// ProtectionAcknowledged records each time the operator let Tenant start
	// without a process protection the system could not apply (TEN-535):
	// which control, the system's reason, and for which version of Tenant. A
	// start that meets the same three goes ahead with a warning; a different
	// control, reason or version is asked about again. Tenant writes it when
	// the operator answers yes at a terminal. No agent tool writes it, and the
	// file tools refuse this file.
	ProtectionAcknowledged []ProtectionAck `json:"protection_acknowledged,omitempty"`
	// FleetSchemes holds each fleet machine's approval scheme, keyed by the id
	// the master gave the machine: an object with the same two keys as above,
	// "permissions" (category modes) and "tool_rules" (tool and command rules).
	// "Always" on a machine's card saves here, under that machine alone. Each
	// entry is kept as written and read by internal/approval, which is stricter
	// than for the master's own keys: an entry with anything it can't read
	// leaves that machine with no scheme, so every call to it is refused until
	// the entry is fixed. Editable by hand, re-read like tool_rules.
	FleetSchemes map[string]json.RawMessage `json:"fleet_schemes,omitempty"`
	// FleetRules are the tool and command rules every newly confirmed fleet
	// machine starts with, laid over the scheme it inherits (pattern → mode, as
	// tool_rules). Changing them moves no machine that already has a scheme.
	// Kept as written, like a FleetSchemes entry: unreadable, no new machine
	// can be given a scheme.
	FleetRules json.RawMessage `json:"fleet_rules,omitempty"`
}

// ProtectionAck is one entry under "protection_acknowledged".
type ProtectionAck struct {
	Control string    `json:"control"` // the control's id, such as linux.dumpable
	Reason  string    `json:"reason"`  // why the system could not apply it
	Version string    `json:"version"` // the version of Tenant it was acknowledged for
	At      time.Time `json:"at"`      // when the operator said yes
}

// maxProtectionAcks bounds the list: an install where a control can never
// apply adds one entry per version, and the older versions' entries are what
// a rollback starts on.
const maxProtectionAcks = 16

// Acknowledged returns the acknowledgement of control failing for reason in
// version, when there is one. All three must match.
func (s *Settings) Acknowledged(control, reason, version string) (ProtectionAck, bool) {
	for _, a := range s.ProtectionAcknowledged {
		if a.Control == control && a.Reason == reason && a.Version == version {
			return a, true
		}
	}
	return ProtectionAck{}, false
}

// Acknowledge records that the operator let version start without control,
// which the system refused for reason. One already recorded keeps its time.
// The newest maxProtectionAcks are kept.
func (s *Settings) Acknowledge(control, reason, version string, at time.Time) {
	if _, ok := s.Acknowledged(control, reason, version); ok {
		return
	}
	s.ProtectionAcknowledged = append(s.ProtectionAcknowledged, ProtectionAck{Control: control, Reason: reason, Version: version, At: at.UTC().Truncate(time.Second)})
	if n := len(s.ProtectionAcknowledged) - maxProtectionAcks; n > 0 {
		s.ProtectionAcknowledged = s.ProtectionAcknowledged[n:]
	}
}

// ExecProfile is one operator override under "exec_profiles", keyed by a
// surface or "default" (every surface without its own entry). Unset fields keep
// the built-in profile: the restricted backend in dry run (network none for
// cron-shell) on macOS, or the host where there is no restricted backend. An
// entry that names a backend keeps the older defaults instead: enforce on,
// network inherit.
// deny_roots ADD to the built-in deny set; nothing can remove Tenant's own dirs
// from it.
type ExecProfile struct {
	Backend    string            `json:"backend,omitempty"`     // host | restricted | container
	Enforce    *bool             `json:"enforce,omitempty"`     // false = dry-run; unset: see above
	Network    string            `json:"network,omitempty"`     // inherit | none | full (allowlist: rejected until enforced)
	Allow      []string          `json:"allow,omitempty"`       // reserved for a future enforced allowlist; rejected today
	ReadRoots  []string          `json:"read_roots,omitempty"`  // empty = whole host readable (deny roots still mask)
	WriteRoots []string          `json:"write_roots,omitempty"` // writable, in addition to the work dir
	DenyRoots  []string          `json:"deny_roots,omitempty"`  // added to the built-in deny set
	EnvPass    []string          `json:"env_pass,omitempty"`    // env: names to forward beyond the safe base set
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
