package privacy

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// preview fills the plan's items from a selection, reading every store
// read-only and computing the same cascades Apply will.
func (p *Plan) preview(ctx context.Context, s *selection) error {
	var items []Item
	add := func(it Item) {
		if it.Count > 0 {
			items = append(items, it)
		}
	}
	// Episodes.
	if len(s.episodes) > 0 {
		it := Item{What: "episodes (conversation turns)", Action: "delete", Count: len(s.episodes)}
		err := withDB(p.d.path(episodesDB), true, func(db *sql.DB) error {
			ids := sortedIDs(s.episodes)
			if len(ids) > 3 {
				ids = ids[:3]
			}
			rows, err := db.QueryContext(ctx, `SELECT id, ts, prompt FROM episodes WHERE id IN (`+inClause(len(ids))+`) ORDER BY id`, anys(ids)...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id, ts int64
				var prompt string
				if err := rows.Scan(&id, &ts, &prompt); err != nil {
					return err
				}
				it.Samples = append(it.Samples, fmt.Sprintf("#%d %s %q", id, time.Unix(ts, 0).Format("2006-01-02"), clip(prompt, 60)))
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}
		add(it)
	}
	// Facts and SME sections.
	err := withDB(p.d.path(factsDB), true, func(db *sql.DB) error {
		changes, err := factChanges(ctx, db, s)
		if err != nil {
			return err
		}
		del := Item{What: "facts", Action: "delete"}
		unlink := Item{What: "facts kept, with a removed source recorded", Action: "unlink"}
		gone := map[int64]bool{}
		pinned := 0
		for _, c := range changes {
			if c.delete {
				gone[c.id] = true
				del.Count++
				if c.pinned {
					pinned++
				}
				if len(del.Samples) < 3 {
					del.Samples = append(del.Samples, fmt.Sprintf("#%d %q", c.id, clip(c.text, 60)))
				}
			} else {
				unlink.Count++
				if len(unlink.Samples) < 3 {
					unlink.Samples = append(unlink.Samples, fmt.Sprintf("#%d %q", c.id, clip(c.text, 60)))
				}
			}
		}
		if pinned > 0 {
			del.What = fmt.Sprintf("facts (%d pinned or protected)", pinned)
		}
		add(del)
		add(unlink)
		smes, err := smeChanges(ctx, db, s, gone)
		if err != nil {
			return err
		}
		smeDel := Item{What: "SME section versions", Action: "delete"}
		smeUnlink := Item{What: "SME section versions citing a removed fact", Action: "unlink"}
		for _, c := range smes {
			if c.delete {
				smeDel.Count++
				if len(smeDel.Samples) < 3 {
					smeDel.Samples = append(smeDel.Samples, c.section)
				}
			} else {
				smeUnlink.Count++
			}
		}
		add(smeDel)
		add(smeUnlink)
		return nil
	})
	if err != nil {
		return err
	}
	// Skills.
	err = withDB(p.d.path(skillsDB), true, func(db *sql.DB) error {
		changes, err := skillChanges(ctx, db, s)
		if err != nil {
			return err
		}
		del := Item{What: "skills (with their history)", Action: "delete"}
		unlink := Item{What: "skills citing a removed run", Action: "unlink"}
		for _, c := range changes {
			if c.delete {
				del.Count++
				if len(del.Samples) < 3 {
					del.Samples = append(del.Samples, c.name)
				}
			} else {
				unlink.Count++
			}
		}
		add(del)
		add(unlink)
		hist, err := skillHistoryMatches(ctx, db, s)
		if err != nil {
			return err
		}
		add(Item{What: "earlier skill versions mentioning the text", Action: "delete", Count: len(hist)})
		return nil
	})
	if err != nil {
		return err
	}
	// Runs, and the episodes that cite them.
	add(Item{What: "run records", Action: "delete", Count: len(s.runs)})
	err = withDB(p.d.path(episodesDB), true, func(db *sql.DB) error {
		ids, err := episodesCitingRuns(ctx, db, s)
		if err != nil {
			return err
		}
		add(Item{What: "episodes citing a removed run", Action: "unlink", Count: len(ids)})
		return nil
	})
	if err != nil {
		return err
	}
	// Usage rows.
	if !s.usageBefore.IsZero() {
		err = withDB(p.d.path(usageDB), true, func(db *sql.DB) error {
			q, args := usageWhere(s)
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM token_usage WHERE `+q, args...).Scan(&n); err != nil {
				return err
			}
			add(Item{What: "token usage rows", Action: "delete", Count: n})
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Archive.
	arch, err := archiveChanges(p.d.DataDir, s)
	if err != nil {
		return err
	}
	months := Item{What: "archive months", Action: "delete"}
	files := Item{What: "archive session files", Action: "delete"}
	events := Item{What: "archive events, from session files that are rewritten", Action: "delete"}
	for _, c := range arch {
		switch {
		case c.removeDir:
			months.Count++
			months.Samples = appendSample(months.Samples, filepath.Base(c.path))
		case c.removeFile:
			files.Count++
			files.Samples = appendSample(files.Samples, filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(c.path)), filepath.Base(c.path))))
		default:
			events.Count += c.drop
			for _, sm := range c.samples {
				events.Samples = appendSample(events.Samples, sm)
			}
		}
	}
	add(months)
	add(files)
	add(events)
	// Files by class.
	byClass := pathsByClass(p.d, s.paths)
	for _, c := range []Class{Research, Screenshots, RelayInbox, EvalArtifacts} {
		it := Item{What: string(c), Action: "delete", Count: len(byClass[c])}
		for _, path := range byClass[c] {
			it.Samples = appendSample(it.Samples, filepath.Base(path))
		}
		add(it)
	}
	// Profile lines.
	lines := 0
	for _, f := range s.profiles {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			if containsFold(l, s.match) {
				lines++
			}
		}
	}
	add(Item{What: "lines of the synthesized user profile", Action: "delete", Count: lines})
	p.Items = items
	return nil
}

func appendSample(samples []string, s string) []string {
	if len(samples) < 3 {
		return append(samples, s)
	}
	return samples
}

// usageWhere is the token_usage filter for a selection.
func usageWhere(s *selection) (string, []any) {
	q := `ts < ?`
	args := []any{s.usageBefore.UTC().Format(time.RFC3339)}
	if s.agent != "" {
		q += ` AND agent_id = ?`
		args = append(args, s.agent)
	}
	return q, args
}

// notes adds what a forget cannot reach on its own.
func (p *Plan) notes(sel Selector, s *selection) {
	p.Notes = append(p.Notes, "backups made before now still hold this data; delete or re-make them if that matters")
	if s.match == "" {
		p.Notes = append(p.Notes, "the synthesized user profile may still mention forgotten conversations; refresh it with /profile refresh")
	}
	if s.match != "" && p.d.ConfigDir != "" {
		souls, _ := filepath.Glob(filepath.Join(p.d.ConfigDir, "soul", "*.toml"))
		for _, f := range souls {
			if b, err := os.ReadFile(f); err == nil && containsFold(string(b), s.match) {
				p.Notes = append(p.Notes, fmt.Sprintf("%s mentions it; the soul is yours to edit (/soul, or the file itself)", f))
			}
		}
	}
	if s.match != "" {
		if runs := researchMentioning(p.d, s.match); len(runs) > 0 {
			p.Notes = append(p.Notes, fmt.Sprintf("research runs mention it: %s; delete them with /research delete <id>", strings.Join(runs, ", ")))
		}
	}
	p.Notes = append(p.Notes, "the log book (logs/) keeps each day's lines until its own retention (logs.retention_days) removes them")
	if sel.Agent != "" {
		p.Notes = append(p.Notes, "limited to agent "+sel.Agent+"; research runs, screenshots, relay files and eval reports belong to no agent and are left alone")
	}
}

// researchMentioning lists research runs with a file containing match.
func researchMentioning(d Data, match string) []string {
	root := d.path(researchDir)
	runs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range runs {
		if !r.IsDir() {
			continue
		}
		found := false
		_ = filepath.WalkDir(filepath.Join(root, r.Name()), func(path string, e os.DirEntry, err error) error {
			if err != nil || found || e.IsDir() {
				return nil
			}
			if b, rerr := os.ReadFile(path); rerr == nil && containsFold(string(b), match) {
				found = true
			}
			return nil
		})
		if found {
			out = append(out, r.Name())
		}
	}
	return out
}
