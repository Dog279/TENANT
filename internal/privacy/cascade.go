package privacy

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// factChange is what a plan does to one fact.
type factChange struct {
	id     int64
	text   string
	pinned bool
	delete bool
	// For a kept fact, the sources that remain and the ones removed.
	keepEpisodes, lostEpisodes []int64
	keepRuns, lostRuns         []string
}

// factChanges works out which facts go and which lose a source: the facts a
// selection names, and every fact citing a removed episode or run. Under a
// forget (derive), a fact every episode source of which is removed goes too;
// otherwise it stays and records what it lost.
func factChanges(ctx context.Context, q queryer, s *selection) ([]factChange, error) {
	pinned := map[int64]bool{}
	if ok, err := hasTable(ctx, q, "fact_signals"); err != nil {
		return nil, err
	} else if ok {
		if err := selectIDs(ctx, q, `SELECT fact_id FROM fact_signals WHERE pinned = 1 OR protected = 1`, nil, pinned); err != nil {
			return nil, err
		}
	}
	runsCol := "NULL"
	if hasColumn(ctx, q, "facts", "source_run_ids") {
		runsCol = "source_run_ids"
	}
	rows, err := q.QueryContext(ctx, `SELECT id, fact, source_episodes, `+runsCol+` FROM facts`)
	if err != nil {
		return nil, fmt.Errorf("privacy: facts: %w", err)
	}
	defer rows.Close()
	var out []factChange
	for rows.Next() {
		var id int64
		var text string
		var epsJSON, runsJSON sql.NullString
		if err := rows.Scan(&id, &text, &epsJSON, &runsJSON); err != nil {
			return nil, err
		}
		eps, err := int64s(epsJSON)
		if err != nil {
			return nil, fmt.Errorf("privacy: fact %d sources: %w", id, err)
		}
		runs, err := strs(runsJSON)
		if err != nil {
			return nil, fmt.Errorf("privacy: fact %d runs: %w", id, err)
		}
		c := factChange{id: id, text: text, pinned: pinned[id]}
		for _, e := range eps {
			if _, gone := s.episodes[e]; gone {
				c.lostEpisodes = append(c.lostEpisodes, e)
			} else {
				c.keepEpisodes = append(c.keepEpisodes, e)
			}
		}
		for _, r := range runs {
			if s.runs[r] {
				c.lostRuns = append(c.lostRuns, r)
			} else {
				c.keepRuns = append(c.keepRuns, r)
			}
		}
		switch {
		case s.facts[id]:
			c.delete = true
		case len(c.lostEpisodes)+len(c.lostRuns) == 0:
			continue
		case s.derive && len(eps) > 0 && len(c.keepEpisodes) == 0:
			c.delete = true
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// smeChange is what a plan does to one SME section version.
type smeChange struct {
	id        int64
	section   string
	delete    bool
	keepFacts []int64
}

// smeChanges finds the SME section versions that cite a removed fact. Under a
// forget, a version citing only removed facts goes; others drop the citation.
func smeChanges(ctx context.Context, q queryer, s *selection, goneFacts map[int64]bool) ([]smeChange, error) {
	if ok, err := hasTable(ctx, q, "sme_docs"); err != nil || !ok {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, section, source_fact_ids FROM sme_docs`)
	if err != nil {
		return nil, fmt.Errorf("privacy: sme: %w", err)
	}
	defer rows.Close()
	var out []smeChange
	for rows.Next() {
		var id int64
		var section string
		var factsJSON sql.NullString
		if err := rows.Scan(&id, &section, &factsJSON); err != nil {
			return nil, err
		}
		facts, err := int64s(factsJSON)
		if err != nil {
			return nil, fmt.Errorf("privacy: sme %d sources: %w", id, err)
		}
		c := smeChange{id: id, section: section}
		lost := 0
		for _, f := range facts {
			if goneFacts[f] {
				lost++
			} else {
				c.keepFacts = append(c.keepFacts, f)
			}
		}
		switch {
		case s.sme[id]:
			c.delete = true
		case lost == 0:
			continue
		case s.derive && len(c.keepFacts) == 0:
			c.delete = true
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// skillChange is what a plan does to one skill.
type skillChange struct {
	id       int64
	name     string
	delete   bool
	keepRuns []string
}

// skillChanges lists the selected skills and every skill citing a removed
// run. A skill is an operator-visible recipe, so losing its source runs only
// unlinks them; forgetting a skill is always explicit (by name or text).
func skillChanges(ctx context.Context, q queryer, s *selection) ([]skillChange, error) {
	runsCol := "NULL"
	if hasColumn(ctx, q, "skills", "source_run_ids") {
		runsCol = "source_run_ids"
	}
	rows, err := q.QueryContext(ctx, `SELECT id, name, `+runsCol+` FROM skills`)
	if err != nil {
		return nil, fmt.Errorf("privacy: skills: %w", err)
	}
	defer rows.Close()
	var out []skillChange
	for rows.Next() {
		var id int64
		var name string
		var runsJSON sql.NullString
		if err := rows.Scan(&id, &name, &runsJSON); err != nil {
			return nil, err
		}
		c := skillChange{id: id, name: name}
		if s.skills[id] {
			c.delete = true
			out = append(out, c)
			continue
		}
		runs, err := strs(runsJSON)
		if err != nil {
			// A malformed provenance list is not this change's to fix.
			continue
		}
		lost := 0
		for _, r := range runs {
			if s.runs[r] {
				lost++
			} else {
				c.keepRuns = append(c.keepRuns, r)
			}
		}
		if lost > 0 {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// skillHistoryMatches lists skill_history rows of kept skills whose earlier
// text contains the forgotten text.
func skillHistoryMatches(ctx context.Context, q queryer, s *selection) (map[int64]bool, error) {
	out := map[int64]bool{}
	if s.match == "" {
		return out, nil
	}
	if ok, err := hasTable(ctx, q, "skill_history"); err != nil || !ok {
		return out, err
	}
	query := `SELECT id FROM skill_history WHERE instr(lower(name || char(10) || prior_description || char(10) || prior_recipe), ?) > 0`
	args := []any{s.match}
	if s.agent != "" {
		query += ` AND agent_id = ?`
		args = append(args, s.agent)
	}
	return out, selectIDs(ctx, q, query, args, out)
}

// episodesCitingRuns lists kept episodes whose run was removed.
func episodesCitingRuns(ctx context.Context, q queryer, s *selection) ([]int64, error) {
	if len(s.runs) == 0 || !hasColumn(ctx, q, "episodes", "source_run_id") {
		return nil, nil
	}
	var out []int64
	runs := sortedKeys(s.runs)
	for _, chunk := range chunks(runs, 500) {
		ids := map[int64]bool{}
		err := selectIDs(ctx, q, `SELECT id FROM episodes WHERE source_run_id IN (`+inClause(len(chunk))+`)`, anys(chunk), ids)
		if err != nil {
			return nil, err
		}
		for id := range ids {
			if _, gone := s.episodes[id]; !gone {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

func idString(id int64) string { return strconv.FormatInt(id, 10) }
