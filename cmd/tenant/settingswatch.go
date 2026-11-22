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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/approval"
	"tenant/internal/lifecycle"
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
func saveSettingsOrTell(stg *settings, cfgDir, agentID string, tell func(string)) {
	if err := stg.Save(cfgDir, agentID); err != nil {
		path := settingsPath(cfgDir, agentID)
		sysNotSaved.Log(context.Background(), "what", filepath.Base(path), "path", path, "err", err)
		tell(errNotSaved("settings: the change", err).Error())
	}
}

// persistModes is the broker's persist hook: stg and the file change in one
// hold of mu, so the settings watcher, which reads the file in a hold of its
// own, never sees one without the other. Split, a watcher tick between them
// read the older file as someone's edit and reloaded it: "Keep them at Ask"
// came undone (C-4).
func persistModes(stg *settings, mu *sync.Mutex, cfgDir, agentID string, tell func(string)) func(map[string]string, []string) {
	return func(modes map[string]string, known []string) {
		mu.Lock()
		defer mu.Unlock()
		stg.Permissions, stg.KnownCategories = modes, known
		saveSettingsOrTell(stg, cfgDir, agentID, tell)
	}
}

// watchSettings runs until ctx is done. stg + mu are the live settings object
// the process saves from (kept in sync so a later save doesn't undo the edit).
func watchSettings(ctx context.Context, cfgDir, agentID string, broker *approvalBroker, rules *toolRules, mu *sync.Mutex, stg *settings, notify func(string)) {
	comp := lifecycle.Track(ctx, "settings watcher", lifecycle.Subsystem, "system", lifecycle.Running, nil)
	defer comp.Set(lifecycle.Stopped, nil)
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
				notify("permissions: " + filepath.Base(path) + " edited — " + changed)
			}
		}
	}
}

// applySettingsFile re-reads the file and applies the tool rules + category
// modes that differ from the live state. Returns a summary of what changed
// ("" when nothing did), or why the file was not applied. Both are filed in
// the system log.
func applySettingsFile(cfgDir, agentID string, broker *approvalBroker, rules *toolRules, mu *sync.Mutex, stg *settings) string {
	path := settingsPath(cfgDir, agentID)
	// The file and the marker it is compared with are read in one hold, so a
	// save in between can't make the process's own write look like an edit.
	mu.Lock()
	fresh, err := loadSettings(cfgDir, agentID)
	savedKnown := slices.Clone(stg.KnownCategories)
	savedModes := maps.Clone(stg.Permissions)
	// The fleet's schemes (fleet_schemes, fleet_rules) are applied in the same
	// hold as the read: the fleet's own saves write the file in a hold of mu
	// too (loadFleetSettings), so what was just read is what the fleet last
	// wrote unless someone edited it. stg tracks the file, entries that could
	// not be read included, so a later save keeps the operator's text.
	var fleetChanged int
	var fleetProblems []string
	if err == nil {
		fleetChanged, fleetProblems = defaultFleet.Load(fresh.FleetSchemes, fresh.FleetRules)
		stg.FleetSchemes, stg.FleetRules = fresh.FleetSchemes, fresh.FleetRules
	}
	mu.Unlock()
	if err != nil {
		sysConfigNotLoaded.Log(context.Background(), "path", path, "fallback", "the live settings stay as they were", "err", err)
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
			pm, ok := parseMode(mode)
			if !ok || live[cat] == "" || live[cat] == pm.String() {
				continue
			}
			// An edit is a file that differs from what this process last saved,
			// not from the live broker: the broker changes a mode just before its
			// persist hook writes it, and a tick in between would read the older
			// file as an edit and undo the change (TEN-428).
			if saved, sok := parseMode(savedModes[cat]); sok && saved == pm {
				continue
			}
			diff++
		}
		// The known_categories marker (TEN-343) reloads with the modes; stg
		// tracks the file's, so the process's own saves compare equal.
		marker := !slices.Equal(fresh.KnownCategories, savedKnown)
		if diff > 0 || marker {
			broker.LoadModes(fresh.Permissions, fresh.KnownCategories)
			mu.Lock()
			stg.Permissions, stg.KnownCategories = broker.Modes(), fresh.KnownCategories
			mu.Unlock()
		}
		if diff > 0 {
			parts = append(parts, fmt.Sprintf("%d category mode(s)", diff))
		}
		if marker {
			parts = append(parts, "the known categories")
		}
	}
	// Exec profiles: keep stg tracking the FILE (invalid entries included) so a
	// later save never clobbers the operator's hand edit, and reload the live set
	// only when its valid content changed.
	// The acknowledgements of a missing process protection too (TEN-535):
	// `tenant service restart` and `tenant update` record one from another
	// process, and this one's next save must not drop it.
	// ponytail: a save by this process before the next tick (3 s) still
	// drops it, and Tenant then asks again; merge on save if that ever bites.
	mu.Lock()
	stg.ExecProfiles, stg.ExecExceptions = fresh.ExecProfiles, fresh.ExecExceptions
	stg.ProtectionAcknowledged = fresh.ProtectionAcknowledged
	mu.Unlock()
	if wanted, _ := validExecProfiles(fresh.ExecProfiles); !execProfilesEqual(wanted, defaultExecProfiles.Snapshot()) {
		n, skipped := defaultExecProfiles.Load(fresh.ExecProfiles)
		s := fmt.Sprintf("%d exec profile(s)", n)
		if len(skipped) > 0 {
			s += fmt.Sprintf(" (%d invalid skipped: %s)", len(skipped), strings.Join(skipped, "; "))
		}
		parts = append(parts, s)
	}
	if wanted, _ := validExecExceptions(fresh.ExecExceptions); !execExceptionsEqual(wanted, defaultExecProfiles.ExceptionsSnapshot()) {
		n, skipped := defaultExecProfiles.LoadExceptions(fresh.ExecExceptions)
		s := fmt.Sprintf("%d sandbox exception(s)", n)
		if len(skipped) > 0 {
			s += fmt.Sprintf(" (%d invalid skipped: %s)", len(skipped), strings.Join(skipped, "; "))
		}
		parts = append(parts, s)
	}
	if fleetChanged > 0 {
		s := fmt.Sprintf("%d fleet scheme change(s)", fleetChanged)
		if len(fleetProblems) > 0 { // a machine whose entry can't be read is refused everything: say which and why
			s += " (" + strings.Join(fleetProblems, " ") + ")"
		}
		parts = append(parts, s)
	}
	changed := strings.Join(parts, ", ")
	if changed != "" {
		sysSettingsApplied.Log(context.Background(), "path", path, "changed", changed)
	}
	return changed
}
