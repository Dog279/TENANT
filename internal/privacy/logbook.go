package privacy

import (
	"bufio"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"tenant/internal/fsperm"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The log book (docs/design/logging-v2.md §6.5, TEN-374). A forget removes
// the events of the sessions and runs it removes: from logbook.db, from the
// text files beside it and from its archives. In the security log a record
// is tombstoned rather than deleted, so its chain still verifies.

// What the log book's plan items say.
const (
	logbookEvents     = "log book events in "
	logbookTombstones = "log book security records (message and data cleared; the record stays so its chain verifies)"
	logbookLines      = "lines in the log book's text files (blanked in place)"
	logbookArchived   = "archived log book events"
	logbookArchTombs  = "archived log book security records (message and data cleared)"
)

func (d Data) logsDir() string { return cmp.Or(d.LogsDir, d.path("logs")) }

// withLogbook opens logbook.db and runs fn; a log book that does not exist is
// skipped, never created.
func withLogbook(d Data, fn func(*store.Store) error) error {
	path := filepath.Join(d.logsDir(), "logbook.db")
	if !exists(path) {
		return nil
	}
	st, err := store.Open(path, store.Options{})
	if err != nil {
		return fmt.Errorf("privacy: log book: %w", err)
	}
	defer st.Close()
	return fn(st)
}

// logbookMatch selects what a selection removes from the log book: the events
// of its sessions and of its runs.
func logbookMatch(s *selection) (store.Match, bool) {
	m := store.Match{Sessions: s.sessions, Traces: s.runs}
	return m, len(m.Sessions)+len(m.Traces) > 0
}

func lineMatch(m store.Match) func(logs.Entry) bool {
	return func(e logs.Entry) bool { return m.Selects(e.Field("session"), e.Field("trace")) }
}

// logbookItems previews what a selection removes from the log book: its
// events per log, with samples, then the text lines and archived events.
func logbookItems(ctx context.Context, d Data, s *selection) ([]Item, error) {
	m, ok := logbookMatch(s)
	if !ok {
		return nil, nil
	}
	byLog := map[string]*Item{}
	tombs := Item{What: logbookTombstones, Action: "tombstone"}
	err := withLogbook(d, func(st *store.Store) error {
		return st.Matching(ctx, m, func(e event.Event) error {
			it := &tombs
			if e.Log != store.SecurityLog {
				if it = byLog[e.Log]; it == nil {
					it = &Item{What: logbookEvents + e.Log, Action: "delete"}
					byLog[e.Log] = it
				}
			}
			it.Count++
			it.Samples = appendSample(it.Samples, fmt.Sprintf("%s %q", e.Time.Local().Format("2006-01-02 15:04"), clip(e.Msg, 60)))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, it := range byLog {
		items = append(items, *it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].What < items[j].What })
	lines, err := logs.ForgetLines(d.logsDir(), lineMatch(m), false)
	if err != nil {
		return nil, fmt.Errorf("privacy: log book text files: %w", err)
	}
	dropped, archTombs, err := store.ForgetArchived(filepath.Join(d.logsDir(), "archive"), m, false)
	if err != nil {
		return nil, fmt.Errorf("privacy: log book archives: %w", err)
	}
	return append(items, tombs,
		Item{What: logbookLines, Action: "blank", Count: lines},
		Item{What: logbookArchived, Action: "delete", Count: dropped},
		Item{What: logbookArchTombs, Action: "tombstone", Count: archTombs}), nil
}

// forgetLogbook removes a selection from the log book: the store by the house
// recipe (store.Forget), then the text files and the archives.
func forgetLogbook(ctx context.Context, d Data, s *selection, res *Result, count func(what, action string, n int)) error {
	m, ok := logbookMatch(s)
	if !ok {
		return nil
	}
	err := withLogbook(d, func(st *store.Store) error {
		r, err := st.Forget(ctx, m)
		logNames := make([]string, 0, len(r.Deleted))
		for l := range r.Deleted {
			logNames = append(logNames, l)
		}
		sort.Strings(logNames)
		for _, l := range logNames {
			count(logbookEvents+l, "delete", r.Deleted[l])
		}
		count(logbookTombstones, "tombstone", r.Tombstoned)
		if r.Busy {
			res.Warnings = append(res.Warnings, "logbook.db: a running Tenant is still reading it, so earlier copies of deleted pages stay in its WAL until its next checkpoint")
		}
		return err
	})
	if err != nil {
		return err
	}
	lines, err := logs.ForgetLines(d.logsDir(), lineMatch(m), true)
	count(logbookLines, "blank", lines)
	if err != nil {
		return fmt.Errorf("privacy: log book text files: %w", err)
	}
	dropped, tombs, err := store.ForgetArchived(filepath.Join(d.logsDir(), "archive"), m, true)
	count(logbookArchived, "delete", dropped)
	count(logbookArchTombs, "tombstone", tombs)
	if err != nil {
		return fmt.Errorf("privacy: log book archives: %w", err)
	}
	return nil
}

// exportLogbook writes the log book's events to dst, one JSON object per
// line, sensitive fields included: every event, or with agent set, those of
// the agent's sessions and runs. A missing log book is fs.ErrNotExist.
func exportLogbook(ctx context.Context, d Data, agent, dst string) (int, error) {
	n := 0
	if !exists(filepath.Join(d.logsDir(), "logbook.db")) {
		return 0, fs.ErrNotExist
	}
	f, err := fsperm.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	write := func(e event.Event) error { n++; return enc.Encode(e) }
	err = withLogbook(d, func(st *store.Store) error {
		if agent != "" {
			m, err := agentMatch(ctx, d, agent)
			if err != nil {
				return err
			}
			return st.Matching(ctx, m, write)
		}
		var from store.Cursor
		for {
			evs, next, _, err := st.Since(ctx, from, 1000)
			if err != nil || len(evs) == 0 {
				return err
			}
			for _, e := range evs {
				if err := write(e); err != nil {
					return err
				}
			}
			from = next
		}
	})
	if ferr := w.Flush(); err == nil {
		err = ferr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// agentMatch selects an agent's sessions and runs, from its turns and its run
// records.
func agentMatch(ctx context.Context, d Data, agent string) (store.Match, error) {
	m := store.Match{Sessions: map[string]bool{}, Traces: map[string]bool{}}
	err := withDB(d.path(episodesDB), true, func(db *sql.DB) error {
		return selectStrings(ctx, db, `SELECT DISTINCT session_id FROM episodes WHERE agent_id = ? AND session_id <> ''`, []any{agent}, m.Sessions)
	})
	if err != nil {
		return m, err
	}
	err = withDB(d.path(runsDB), true, func(db *sql.DB) error {
		if err := selectStrings(ctx, db, `SELECT DISTINCT session_id FROM runs WHERE agent_id = ? AND session_id <> ''`, []any{agent}, m.Sessions); err != nil {
			return err
		}
		return selectStrings(ctx, db, `SELECT trace_id FROM runs WHERE agent_id = ?`, []any{agent}, m.Traces)
	})
	return m, err
}
