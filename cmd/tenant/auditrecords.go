package main

import (
	"context"
	"maps"
	"path/filepath"
	"slices"

	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// The Security log's records of changes (docs/design/logging-v2.md §10.1,
// TEN-398) are written where the change is handled, which knows who made it
// (the actor) and from where (the surface). A record that fails to commit
// is the log book's to report (§10.3); the change itself stands, so the
// callers ignore Audit's error.

const (
	actorOperator = "operator" // the person running Tenant
	actorAgent    = "agent"    // the agent, through one of its tools
	actorTenant   = "tenant"   // Tenant itself, on a schedule

	surfaceTUI       = "tui"
	surfaceDashboard = "dashboard"
	// surfaceDashboardRemote is a fleet machine confirmed from a signed-in
	// browser that is not on this computer (TEN-545; the dashboard's own
	// refusals of one carry the same word).
	surfaceDashboardRemote = "dashboard-remote"
	surfaceCLI             = "cli" // a tenant command
	// surfaceEnrollCode is a fleet machine confirmed by Tenant itself as it
	// enrolled, on the strength of its one-time code (TEN-553): no person
	// typed anything, so the record's actor is Tenant.
	surfaceEnrollCode = "enroll-code"
)

// saveCredentials saves creds and files a record for each secret the save
// stores, changes or deletes, by ID only, never the value. The operator's
// writes to credentials.json go through it; token refreshes a plugin makes
// on its own don't.
func saveCredentials(cfgDir string, creds *credentials, surface string) error {
	before, _ := peekCredentials(cfgDir) // unreadable: every secret is new
	if err := creds.Save(cfgDir); err != nil {
		return err
	}
	auditSecrets(context.Background(), before.Secrets, creds.Secrets, surface)
	return nil
}

func auditSecrets(ctx context.Context, before, after map[string]string, surface string) {
	ids := slices.Collect(maps.Keys(after))
	for id := range before {
		if _, ok := after[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		old, had := before[id]
		cur, has := after[id]
		var d *event.Def
		switch {
		case !has:
			d = catalog.SecretDeleted
		case !had:
			d = catalog.SecretStored
		case old != cur:
			d = catalog.SecretChanged
		default:
			continue
		}
		_ = d.Audit(ctx, "secret", id, "actor", actorOperator, "surface", surface)
	}
}

// attachCommandStore gives a one-shot command's log book (fileEvents) the
// store and the Security log's key, as openLogHub does for serve and the
// TUI, so the command's records reach logbook.db and the chain, not only the
// text files. A verify (auditOnly) creates no key.
func attachCommandStore(h *logs.Hub, c *commonFlags) {
	readKey := chainKey
	if c.auditOnly {
		readKey = readChainKey
	}
	key, _ := readKey(c.cfgDir) // none: the store refuses only the security records
	st, err := store.Open(filepath.Join(h.Root(), "logbook.db"), store.Options{BootID: runtrace.BootID(), ChainKey: key})
	if err != nil {
		return // the text files still get them
	}
	h.AttachStore(st)
}
