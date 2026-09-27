package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/store"
)

// Doctor's log book checks (TEN-375). Doctor opens logbook.db read-only,
// with or without --fix: it creates, tightens and migrates nothing there,
// and reads beside a running serve without holding it up (SQLite readers
// never block its writer). What another process is doing it reads from what
// that process leaves: the housekeeper lease, the boots table, the boot
// locks and the mirror-only markers.

// logbookStore is the log book doctor opened, or the result that says why
// there is none.
func (e *doctorEnv) logbookStore() (*store.Store, checkResult) {
	switch {
	case e.logbook != nil:
		return e.logbook, checkResult{}
	case logbookOff(e.c):
		return nil, checkResult{Status: statusSkip, Detail: "logs.store is false: the log book keeps its text files only"}
	case errors.Is(e.logbookErr, fs.ErrNotExist):
		return nil, checkResult{Status: statusSkip, Detail: "logbook.db not created yet (serve and the TUI create it)"}
	}
	return nil, checkResult{Status: statusFail, Detail: fmt.Sprintf("logbook.db could not be opened: %v", e.logbookErr),
		Fix: "check permissions and disk space on " + e.c.logsRoot()}
}

// logbookOff reports config logs.store false.
func logbookOff(c *commonFlags) bool {
	return c.lc != nil && c.lc.Logs.Store != nil && !*c.lc.Logs.Store
}

// checkLogbookIntegrity runs SQLite's quick_check on logbook.db, as the
// housekeeper does daily.
func checkLogbookIntegrity(ctx context.Context, e *doctorEnv) checkResult {
	st, none := e.logbookStore()
	if st == nil {
		return none
	}
	v, _ := st.Meta(ctx, "schema_version")
	schema := fmt.Sprintf("schema version %s (this build's: %d)", v, store.SchemaVersion())
	switch err := st.Check(ctx); {
	case errors.Is(err, store.ErrCorrupt):
		return checkResult{Status: statusFail, Detail: err.Error(),
			Fix: "stop every Tenant process, then start one: it moves logbook.db aside (logbook.db.corrupt-<time>) and starts a new store; the text files keep every event"}
	case err != nil:
		return checkResult{Status: statusWarn, Detail: "quick_check could not run: " + err.Error()}
	}
	return checkResult{Status: statusOK, Detail: "quick_check OK; " + schema}
}

// checkLogbookSize reports logbook.db and its WAL against logs.budget_mb,
// and each log against its limit.
func checkLogbookSize(ctx context.Context, e *doctorEnv) checkResult {
	st, none := e.logbookStore()
	if st == nil {
		return none
	}
	var cfg logsConfig
	if e.c.lc != nil {
		cfg = e.c.lc.Logs
	}
	b := logsBudget(cfg)
	db, wal, err := st.Size(ctx)
	fill, ferr := st.Fill(ctx, b)
	pols, perr := st.Policies(ctx, b)
	if err = errors.Join(err, ferr, perr); err != nil {
		return checkResult{Status: statusWarn, Detail: "could not measure logbook.db: " + err.Error()}
	}
	total := db + wal
	detail := fmt.Sprintf("logbook.db %s + WAL %s", humanBytes(db), humanBytes(wal))
	if b.TotalBytes > 0 {
		detail += fmt.Sprintf(" = %s of the %s budget (%.0f%%)", humanBytes(total), humanBytes(b.TotalBytes), 100*float64(total)/float64(b.TotalBytes))
	} else {
		detail += " (logs.budget_mb -1: no budget)"
	}
	names := slices.SortedFunc(maps.Keys(fill), func(a, b string) int { return cmp.Or(cmp.Compare(fill[b], fill[a]), strings.Compare(a, b)) })
	var parts, kept []string
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s %.0f%% of %s", n, 100*fill[n], humanBytes(pols[n].MaxBytes)))
		if fill[n] > 1 && pols[n].OnFull == store.Keep {
			kept = append(kept, n)
		}
	}
	if len(parts) > 0 {
		detail += "; fullest logs: " + strings.Join(parts[:min(len(parts), 5)], ", ")
	}
	switch {
	case b.TotalBytes > 0 && total > b.TotalBytes:
		return checkResult{Status: statusWarn, Detail: detail + "; over its budget",
			Fix: "retention runs in the process holding the housekeeper lease (see the log book housekeeper check); or raise logs.budget_mb"}
	case len(kept) > 0:
		return checkResult{Status: statusWarn, Detail: detail + "; past its limit, kept (on_full keep): " + strings.Join(kept, ", "),
			Fix: "raise its max_mb, or set its on_full to overwrite or archive (logs.per_log)"}
	}
	return checkResult{Status: statusOK, Detail: detail}
}

// checkLogbookHousekeeper reports who holds the housekeeper lease, which
// runs retention and checkpoints in exactly one process, and whether it is
// renewed.
func checkLogbookHousekeeper(ctx context.Context, e *doctorEnv) checkResult {
	st, none := e.logbookStore()
	if st == nil {
		return none
	}
	root := e.c.logsRoot()
	holder, expires, err := st.Lease(ctx, logs.HousekeeperLease)
	if err != nil {
		return checkResult{Status: statusWarn, Detail: err.Error()}
	}
	if holder == "" {
		return checkResult{Status: statusOK, Detail: "no process has held it yet (serve or the TUI takes it)"}
	}
	who := describeBoot(ctx, st, holder)
	left := time.Until(expires).Round(time.Second)
	switch alive := logs.BootAlive(root, holder); {
	case left > 0 && alive:
		return checkResult{Status: statusOK, Detail: fmt.Sprintf("held by %s; runs out in %s unless renewed", who, left)}
	case left > 0:
		return checkResult{Status: statusOK, Detail: fmt.Sprintf("held by %s, which has exited; another process takes it over in %s", who, left)}
	}
	// Expired: a fault only while a process that keeps the log book runs.
	var running []string
	if boots, err := st.Unfinished(ctx, ""); err == nil {
		for _, b := range boots {
			if logs.BootAlive(root, b.ID) {
				running = append(running, describeBoot(ctx, st, b.ID))
			}
		}
	}
	if len(running) > 0 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("expired %s ago (last held by %s) while %s runs: nothing applies retention or checkpoints the WAL", -left, who, strings.Join(running, ", ")),
			Fix:    "see the log book mirror-only check and the logbook log (`tenant logs`); restarting the process takes the lease again"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("expired %s ago, last held by %s; no Tenant process that keeps the log book is running", -left, who)}
}

// describeBoot names a process by its boots row: role, pid, host, boot ID.
func describeBoot(ctx context.Context, st *store.Store, id string) string {
	short := id[:min(len(id), 8)]
	b, err := st.Boot(ctx, id)
	if err != nil {
		return "boot " + short
	}
	return fmt.Sprintf("%s pid %d on %s (boot %s)", firstNonEmpty(b.Role, "a process"), b.PID, b.Host, short)
}

// checkLogbookMirrorOnly reports the processes running on the text files
// alone, their logbook.db unwritten. --fix clears the markers of processes
// that exited that way.
func checkLogbookMirrorOnly(_ context.Context, e *doctorEnv) checkResult {
	if logbookOff(e.c) {
		return checkResult{Status: statusOK, Detail: "logs.store is false: every process keeps the text files only, by configuration"}
	}
	marks, err := logs.MirrorOnlyProcesses(e.c.logsRoot())
	if err != nil {
		return checkResult{Status: statusWarn, Detail: "could not read the mirror-only markers: " + err.Error()}
	}
	var live, gone []string
	cleared := 0
	for _, m := range marks {
		who := fmt.Sprintf("pid %d on %s (boot %s) since %s", m.PID, m.Host, m.Boot[:8], m.Since.Local().Format(time.DateTime))
		switch {
		case m.Running:
			live = append(live, who+": "+m.Why)
		case e.fix && os.Remove(m.Path) == nil:
			cleared++
		default:
			gone = append(gone, who)
		}
	}
	switch {
	case len(live) > 0:
		return checkResult{Status: statusWarn,
			Detail: "running on the text files alone, logbook.db not written: " + strings.Join(live, "; "),
			Fix:    "fix the cause the reason names: the process retries every 30 s and imports the outage's lines once a commit succeeds; one whose logbook.db never opened needs a restart"}
	case len(gone) > 0:
		return checkResult{Status: statusWarn,
			Detail: "exited while on the text files alone (their outage's lines are in the text files only): " + strings.Join(gone, "; "),
			Fix:    "run `tenant doctor --fix` to clear the stale marker(s)"}
	case cleared > 0:
		return checkResult{Status: statusWarn, Fixed: true, Detail: fmt.Sprintf("%d process(es) had exited while on the text files alone", cleared),
			Fix: fmt.Sprintf("cleared %d stale marker(s)", cleared)}
	}
	return checkResult{Status: statusOK, Detail: "no process runs on the text files alone"}
}

// checkLogbookLosses reports the events never written, per log: dropped (a
// full queue, low disk) and suppressed (rate limits), counted since
// logbook.db was created. The logbook log has each minute's report.
func checkLogbookLosses(ctx context.Context, e *doctorEnv) checkResult {
	st, none := e.logbookStore()
	if st == nil {
		return none
	}
	infos, err := st.Logs(ctx)
	if err != nil {
		return checkResult{Status: statusWarn, Detail: err.Error()}
	}
	var dropped, suppressed int64
	var lossy []store.LogInfo
	for _, l := range infos {
		dropped, suppressed = dropped+l.Dropped, suppressed+l.Suppressed
		if l.Dropped+l.Suppressed > 0 {
			lossy = append(lossy, l)
		}
	}
	detail := fmt.Sprintf("%d event(s) dropped (a full queue or low disk), %d suppressed by rate limits, since logbook.db was created", dropped, suppressed)
	slices.SortFunc(lossy, func(a, b store.LogInfo) int { return cmp.Compare(b.Dropped+b.Suppressed, a.Dropped+a.Suppressed) })
	var parts []string
	for _, l := range lossy[:min(len(lossy), 5)] {
		parts = append(parts, fmt.Sprintf("%s %d/%d", l.Name, l.Dropped, l.Suppressed))
	}
	if len(parts) > 0 {
		detail += "; most (dropped/suppressed): " + strings.Join(parts, ", ")
	}
	return checkResult{Status: statusOK, Detail: detail}
}

// checkLogbookLocation reports where the log book lives, and whether a
// relocated one (logs.dir, --log-dir) is in the permission audit.
func checkLogbookLocation(_ context.Context, e *doctorEnv) checkResult {
	root := e.c.logsRoot()
	if filepath.Clean(root) == filepath.Join(e.c.dataDir, "logs") {
		return checkResult{Status: statusOK, Detail: root + " (the default, <data>/logs)"}
	}
	detail := "logs.dir moves the log book to " + root
	if _, err := os.Stat(root); err != nil {
		return checkResult{Status: statusOK, Detail: detail + " (not created yet)"}
	}
	if !isInside(root, e.c.dataDir) {
		detail += "; it is outside the data dir, so `tenant backup` leaves it out"
	}
	if why := logsRootTooBroad(root, e.c.dataDir, e.c.cfgDir); why != "" {
		return checkResult{Status: statusWarn, Detail: detail + "; it " + why + ", so doctor leaves its permissions alone",
			Fix: "give logs.dir a folder of its own"}
	}
	return checkResult{Status: statusOK, Detail: detail + "; the private data permissions check covers it"}
}

// logsRootTooBroad says why a relocated log book root holds more than the
// log book, "" when it doesn't: doctor --fix tightens everything under it.
func logsRootTooBroad(root, dataDir, cfgDir string) string {
	home, _ := os.UserHomeDir()
	for _, d := range []struct{ path, what string }{{home, "your home directory"}, {dataDir, "the data dir"}, {cfgDir, "the config dir"}} {
		if d.path != "" && isInside(d.path, root) {
			return "holds " + d.what
		}
	}
	return ""
}
