package main

// settingswatch.go: settings.<agent>.json is a file people edit — a tool rule
// typed in, a permission flipped from another window, a config push from a
// script. The running hub must honor that without a restart: this watcher
// polls the file's mtime (like watchCredentials) and, when it changed, re-reads
// it and applies what differs from the live state — the tool rules and the
// host broker's category modes. Content is compared, so the process's own
// saves (which write exactly the live state) never cause a reload.

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"tenant/internal/approval"
)

const settingsWatchInterval = 3 * time.Second

func settingsModTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// saveSettingsOrTell saves settings.<agent>.json; the caller holds its mutex.
// Every permission change is saved through it — /permissions, the dashboard's
// rules, an "approve always", a tool switched on or off — so a failure is
// logged and told to the operator: the change is live, but a restart would
// undo it without a word.
func saveSettingsOrTell(stg *settings, cfgDir, agentID string, log *slog.Logger, tell func(string)) {
	if err := stg.Save(cfgDir, agentID); err != nil {
		log.Warn("settings save failed", "err", err)
		tell(errNotSaved("settings: the change", err).Error())
	}
}

// watchSettings runs until ctx is done. stg + mu are the live settings object
// the process saves from (kept in sync so a later save doesn't undo the edit).
func watchSettings(ctx context.Context, cfgDir, agentID string, broker *approvalBroker, rules *toolRules, mu *sync.Mutex, stg *settings, notify func(string)) {
	path := settingsPath(cfgDir, agentID)
	last := settingsModTime(path)
	t := time.NewTicker(settingsWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := settingsModTime(path)
			if cur == last {
				continue
			}
			last = cur
			if changed := applySettingsFile(cfgDir, agentID, broker, rules, mu, stg); changed != "" && notify != nil {
				notify("permissions: " + strings.TrimSuffix(path[strings.LastIndex(path, string(os.PathSeparator))+1:], "") + " edited — " + changed)
			}
		}
	}
}

// applySettingsFile re-reads the file and applies the tool rules + category
// modes that differ from the live state. Returns a summary of what changed
// ("" when nothing did, or the file could not be read).
func applySettingsFile(cfgDir, agentID string, broker *approvalBroker, rules *toolRules, mu *sync.Mutex, stg *settings) string {
	fresh, err := loadSettings(cfgDir, agentID)
	if err != nil {
		return "not applied: " + err.Error()
	}
	var parts []string
	// Compare the file's USABLE rules to the live set, so an invalid entry
	// (skipped on load) doesn't read as a change on every tick.
	if wanted, _ := approval.NormalizeRules(fresh.ToolRules); rules != nil && !maps.Equal(wanted, rules.Snapshot()) {
		n, skipped := rules.Load(fresh.ToolRules)
		mu.Lock()
		stg.ToolRules = rules.Snapshot()
		mu.Unlock()
		s := fmt.Sprintf("%d tool rule(s)", n)
		if skipped > 0 {
			s += fmt.Sprintf(" (%d invalid skipped)", skipped)
		}
		parts = append(parts, s)
	}
	if broker != nil && len(fresh.Permissions) > 0 {
		live := broker.Modes()
		diff := 0
		for cat, mode := range fresh.Permissions {
			if pm, ok := parseMode(mode); ok && live[cat] != "" && live[cat] != pm.String() {
				diff++
			}
		}
		if diff > 0 {
			broker.LoadModes(fresh.Permissions)
			mu.Lock()
			stg.Permissions = broker.Modes()
			mu.Unlock()
			parts = append(parts, fmt.Sprintf("%d category mode(s)", diff))
		}
	}
	// Exec profiles: keep stg tracking the FILE (invalid entries included) so a
	// later save never clobbers the operator's hand edit, and reload the live set
	// only when its valid content changed.
	mu.Lock()
	stg.ExecProfiles = fresh.ExecProfiles
	mu.Unlock()
	if wanted, _ := validExecProfiles(fresh.ExecProfiles); !execProfilesEqual(wanted, defaultExecProfiles.Snapshot()) {
		n, skipped := defaultExecProfiles.Load(fresh.ExecProfiles)
		s := fmt.Sprintf("%d exec profile(s)", n)
		if len(skipped) > 0 {
			s += fmt.Sprintf(" (%d invalid skipped: %s)", len(skipped), strings.Join(skipped, "; "))
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}
