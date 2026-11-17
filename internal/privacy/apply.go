package privacy

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"tenant/internal/fsperm"
	"tenant/internal/logs/event"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
)

// Result is what Apply changed.
type Result struct {
	// Items count what was actually removed or repaired, which can differ
	// from the preview when data changed in between.
	Items []Item
	// Warnings are parts that did not fully finish, such as a WAL a live
	// Tenant was still reading.
	Warnings []string
}

// Apply carries the plan out. It selects afresh, then works store by store,
// each in one transaction: facts first (so no fact is left pointing at a
// source that is already gone), then SME sections, skills, episodes, runs and
// usage, then the archive, files and the user profile.
func (p *Plan) Apply(ctx context.Context) (res *Result, err error) {
	ctx, act := event.Start(ctx, Deletion, "reason", p.Reason)
	defer func() {
		if res != nil { // what was done is done, even when a later store failed
			for _, it := range res.Items {
				Removed.Log(ctx, "data", it.What, "action", it.Action, "count", it.Count, "reason", p.Reason)
			}
			for _, w := range res.Warnings {
				Unfinished.Log(ctx, "warning", w, "reason", p.Reason)
			}
		}
		act.End(&err)
	}()
	s, err := p.build(ctx)
	if err != nil {
		return nil, err
	}
	res = &Result{}
	count := func(what, action string, n int) {
		if n > 0 {
			res.Items = append(res.Items, Item{What: what, Action: action, Count: n})
		}
	}
	// Backups made before now keep what a forget removes; status counts them.
	defer func() {
		if p.Reason == ReasonForget && len(res.Items) > 0 {
			if err := appendLedger(p.d, ledgerLine{At: p.d.now().UTC(), Forget: true}); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("could not record the deletion for `tenant privacy status`: %v", err))
			}
		}
	}()
	removedAt := p.d.now().Unix()

	// Stores opened through their own packages first, so their newest
	// tables (the removed-source records) exist before anything is written.
	if err := ensureSchemas(p.d); err != nil {
		return nil, err
	}

	// Facts, their removed-source records, and SME sections.
	err = p.inTx(ctx, factsDB, res, func(tx *sql.Tx) error {
		changes, err := factChanges(ctx, tx, &s)
		if err != nil {
			return err
		}
		gone := map[int64]bool{}
		deleted, unlinked := 0, 0
		for _, c := range changes {
			if c.delete {
				if _, err := tx.ExecContext(ctx, `DELETE FROM facts WHERE id = ?`, c.id); err != nil {
					return fmt.Errorf("delete fact %d: %w", c.id, err)
				}
				gone[c.id] = true
				deleted++
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE facts SET source_episodes = ?, source_run_ids = ? WHERE id = ?`,
				jsonList(c.keepEpisodes), jsonList(c.keepRuns), c.id); err != nil {
				return fmt.Errorf("unlink fact %d: %w", c.id, err)
			}
			for _, e := range c.lostEpisodes {
				if err := tombstone(ctx, tx, c.id, "episode", idString(e), removedAt, p.Reason); err != nil {
					return err
				}
			}
			for _, r := range c.lostRuns {
				if err := tombstone(ctx, tx, c.id, "run", r, removedAt, p.Reason); err != nil {
					return err
				}
			}
			unlinked++
		}
		count("facts", "delete", deleted)
		count("facts kept, with a removed source recorded", "unlink", unlinked)
		smes, err := smeChanges(ctx, tx, &s, gone)
		if err != nil {
			return err
		}
		smeDeleted, smeUnlinked := 0, 0
		for _, c := range smes {
			if c.delete {
				if _, err := tx.ExecContext(ctx, `DELETE FROM sme_docs WHERE id = ?`, c.id); err != nil {
					return err
				}
				smeDeleted++
			} else {
				if _, err := tx.ExecContext(ctx, `UPDATE sme_docs SET source_fact_ids = ? WHERE id = ?`, jsonList(c.keepFacts), c.id); err != nil {
					return err
				}
				smeUnlinked++
			}
		}
		count("SME section versions", "delete", smeDeleted)
		count("SME section versions citing a removed fact", "unlink", smeUnlinked)
		return nil
	}, "facts_fts")
	if err != nil {
		return res, err
	}

	// Skills and their history.
	err = p.inTx(ctx, skillsDB, res, func(tx *sql.Tx) error {
		changes, err := skillChanges(ctx, tx, &s)
		if err != nil {
			return err
		}
		deleted, unlinked := 0, 0
		for _, c := range changes {
			if c.delete {
				if _, err := tx.ExecContext(ctx, `DELETE FROM skill_history WHERE skill_id = ?`, c.id); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, c.id); err != nil {
					return err
				}
				deleted++
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE skills SET source_run_ids = ? WHERE id = ?`, jsonList(c.keepRuns), c.id); err != nil {
				return err
			}
			unlinked++
		}
		hist, err := skillHistoryMatches(ctx, tx, &s)
		if err != nil {
			return err
		}
		for _, id := range sortedIDs(hist) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM skill_history WHERE id = ?`, id); err != nil {
				return err
			}
		}
		count("skills (with their history)", "delete", deleted)
		count("skills citing a removed run", "unlink", unlinked)
		count("earlier skill versions mentioning the text", "delete", len(hist))
		return nil
	}, "")
	if err != nil {
		return res, err
	}

	// Episodes.
	err = p.inTx(ctx, episodesDB, res, func(tx *sql.Tx) error {
		unlink, err := episodesCitingRuns(ctx, tx, &s)
		if err != nil {
			return err
		}
		for _, id := range unlink {
			if _, err := tx.ExecContext(ctx, `UPDATE episodes SET source_run_id = NULL WHERE id = ?`, id); err != nil {
				return err
			}
		}
		n := 0
		for _, chunk := range chunks(sortedIDs(s.episodes), 500) {
			r, err := tx.ExecContext(ctx, `DELETE FROM episodes WHERE id IN (`+inClause(len(chunk))+`)`, anys(chunk)...)
			if err != nil {
				return err
			}
			k, _ := r.RowsAffected()
			n += int(k)
		}
		count("episodes (conversation turns)", "delete", n)
		count("episodes citing a removed run", "unlink", len(unlink))
		return nil
	}, "episodes_fts")
	if err != nil {
		return res, err
	}

	// The log book, before the runs: their trace IDs pick its events, so a
	// forget that stops here finds them again when it is run again.
	if err := forgetLogbook(ctx, p.d, &s, res, count); err != nil {
		return res, err
	}

	// Runs.
	err = p.inTx(ctx, runsDB, res, func(tx *sql.Tx) error {
		n := 0
		for _, chunk := range chunks(sortedKeys(s.runs), 500) {
			r, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE trace_id IN (`+inClause(len(chunk))+`)`, anys(chunk)...)
			if err != nil {
				return err
			}
			k, _ := r.RowsAffected()
			n += int(k)
		}
		count("run records", "delete", n)
		return nil
	}, "")
	if err != nil {
		return res, err
	}

	// Usage.
	if !s.usageBefore.IsZero() {
		err = p.inTx(ctx, usageDB, res, func(tx *sql.Tx) error {
			q, args := usageWhere(&s)
			r, err := tx.ExecContext(ctx, `DELETE FROM token_usage WHERE `+q, args...)
			if err != nil {
				return err
			}
			k, _ := r.RowsAffected()
			count("token usage rows", "delete", int(k))
			return nil
		}, "")
		if err != nil {
			return res, err
		}
	}

	// Archive.
	arch, err := archiveChanges(p.d.DataDir, &s)
	if err != nil {
		return res, err
	}
	turns := turnsBySession(&s)
	months, files, events := 0, 0, 0
	for _, c := range arch {
		n, err := applyArchiveChange(c, &s, turns)
		if err != nil {
			return res, err
		}
		switch {
		case c.removeDir:
			months++
		case c.removeFile:
			files++
		default:
			events += n
		}
	}
	removeEmptyMonths(p.d.DataDir)
	count("archive months", "delete", months)
	count("archive session files", "delete", files)
	count("archive events, from session files that are rewritten", "delete", events)

	// Files.
	byClass := pathsByClass(p.d, s.paths)
	if _, err := removePaths(p.d, s.paths); err != nil {
		return res, err
	}
	for _, c := range []Class{Research, Screenshots, RelayInbox, EvalArtifacts} {
		count(string(c), "delete", len(byClass[c]))
	}

	// Profile lines.
	lines := 0
	for _, f := range s.profiles {
		n, err := scrubLines(f, s.match)
		if err != nil {
			return res, err
		}
		lines += n
	}
	count("lines of the synthesized user profile", "delete", lines)
	return res, nil
}

// inTx runs fn in one transaction on a store, then optimizes its full-text
// index (so deleted words leave the index, not just the table) and truncates
// its WAL so the old pages do not linger there. A store that does not exist
// is skipped.
func (p *Plan) inTx(ctx context.Context, name string, res *Result, fn func(*sql.Tx) error, fts string) error {
	return withDB(p.d.path(name), false, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("privacy: %s: %w", name, err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("privacy: %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("privacy: %s: %w", name, err)
		}
		if fts != "" {
			if _, err := db.ExecContext(ctx, `INSERT INTO `+fts+`(`+fts+`) VALUES('optimize')`); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not compact the search index: %v", name, err))
			}
		}
		var busy, logFrames, done int
		if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err == nil && busy != 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: a running Tenant is still reading it, so earlier copies of deleted pages stay in its WAL until its next checkpoint", name))
		}
		return nil
	})
}

func tombstone(ctx context.Context, tx *sql.Tx, factID int64, kind, id string, at int64, reason Reason) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fact_source_tombstones (fact_id, source_kind, source_id, removed_at, reason) VALUES (?, ?, ?, ?, ?)`,
		factID, kind, id, at, string(reason))
	if err != nil {
		return fmt.Errorf("record removed source of fact %d: %w", factID, err)
	}
	return nil
}

// ensureSchemas opens the existing stores through their own packages, which
// bring an older database up to the current schema, before any row changes.
func ensureSchemas(d Data) error {
	if path := d.path(factsDB); exists(path) {
		st, err := semantic.Open(path)
		if err != nil {
			return fmt.Errorf("privacy: %w", err)
		}
		st.Close()
	}
	if path := d.path(episodesDB); exists(path) {
		st, err := episodic.Open(path)
		if err != nil {
			return fmt.Errorf("privacy: %w", err)
		}
		st.Close()
	}
	if path := d.path(skillsDB); exists(path) {
		st, err := skills.Open(path)
		if err != nil {
			return fmt.Errorf("privacy: %w", err)
		}
		st.Close()
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// scrubLines removes the lines of a text file that contain match and returns
// how many went.
func scrubLines(path, match string) (int, error) {
	b, err := os.ReadFile(path)
	if missing(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var keep []string
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if containsFold(l, match) {
			n++
			continue
		}
		keep = append(keep, l)
	}
	if n == 0 {
		return 0, nil
	}
	if err := fsperm.Replace(path, []byte(strings.Join(keep, "\n"))); err != nil {
		return 0, err
	}
	return n, nil
}
