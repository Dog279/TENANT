package privacy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ForgetOpen is a forget on an episodes store and a facts store the caller
// has open, such as a fleet machine's memory, which lives in RAM in the
// process that holds the fleet (TEN-532). It selects in the two databases as
// Forget does in the files (sessions, episode ids of this store, runs, text,
// a date, an agent), and removes what was selected and the facts derived
// only from it: rows deleted (the caller turns secure_delete on), provenance
// recorded for the facts that keep other sources, and both full-text indexes
// optimized so no deleted word stays in them. Facts, skills and archive
// months of sel are not this store's and are ignored; so is the file side
// of a forget (archive, attachments, profiles, the log book).
//
// Either database may be nil. Each store is one transaction; the caller must
// not hold another statement open on them.
func ForgetOpen(ctx context.Context, episodes, facts *sql.DB, sel Selector, now time.Time) (*Result, error) {
	sel.Facts, sel.Skills, sel.ArchiveMonths = nil, nil, nil
	if sel.Empty() {
		return nil, fmt.Errorf("privacy: nothing selected to forget")
	}
	match := lowerASCII(strings.TrimSpace(sel.Match))
	if sel.Match != "" && match == "" {
		return nil, fmt.Errorf("privacy: the text to forget is blank")
	}
	s := newSelection()
	s.derive, s.agent, s.match = true, sel.Agent, match
	if episodes != nil {
		if err := selectEpisodeRows(ctx, episodes, sel, &s); err != nil {
			return nil, err
		}
		if err := episodeRuns(ctx, episodes, s.episodes, s.runs); err != nil {
			return nil, err
		}
	}
	for _, r := range sel.Runs {
		s.runs[r] = true
	}
	if facts != nil {
		if err := selectFactRows(ctx, facts, sel, &s); err != nil {
			return nil, err
		}
	}
	res := &Result{}
	count := func(what, action string, n int) {
		if n > 0 {
			res.Items = append(res.Items, Item{What: what, Action: action, Count: n})
		}
	}
	in := func(db *sql.DB, name, fts string, fn func(*sql.Tx) error) error {
		if db == nil {
			return nil
		}
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
		if _, err := db.ExecContext(ctx, `INSERT INTO `+fts+`(`+fts+`) VALUES('optimize')`); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not compact the search index: %v", name, err))
		}
		return nil
	}
	if err := in(facts, "facts", "facts_fts", func(tx *sql.Tx) error {
		return forgetFacts(ctx, tx, &s, now.Unix(), ReasonForget, count)
	}); err != nil {
		return res, err
	}
	err := in(episodes, "episodes", "episodes_fts", func(tx *sql.Tx) error {
		return forgetEpisodes(ctx, tx, &s, count)
	})
	return res, err
}
