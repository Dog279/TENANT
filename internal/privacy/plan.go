package privacy

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Data is one Tenant install.
type Data struct {
	DataDir   string
	ConfigDir string
	// LogsDir is the log book's root (logs.dir); empty means <data>/logs.
	LogsDir string
	// Now is the clock; tests fix it.
	Now func() time.Time
}

func (d Data) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Data) path(parts ...string) string {
	return filepath.Join(append([]string{d.DataDir}, parts...)...)
}

// Store files inside the data dir.
const (
	episodesDB = "episodes.db"
	factsDB    = "facts.db"
	skillsDB   = "skills.db"
	runsDB     = "runs.db"
	usageDB    = "usage.db"
)

// Plan is what a forget or a prune will remove and repair. Build one with
// Forget or Prune, show it, then Apply it.
type Plan struct {
	Reason Reason
	// Items summarize the change, one per kind of data, in a stable order.
	Items []Item
	// Notes are things the plan does not change but the operator should
	// know, such as a soul file that mentions forgotten text.
	Notes []string

	d Data
	// build selects the data afresh. Apply calls it again, so rows written
	// between the preview and the apply (a turn in the session being
	// forgotten, a fact distilled from it) are covered too.
	build func(context.Context) (selection, error)
}

// Item is one line of a plan's preview.
type Item struct {
	// What is the kind of data: "episodes", "facts", "archive events", …
	What string
	// Action is what happens to it: "delete", "rewrite", "unlink".
	Action  string
	Count   int
	Samples []string
}

// Empty reports whether applying the plan would change nothing.
func (p *Plan) Empty() bool { return len(p.Items) == 0 }

// selection is everything a plan targets directly. Apply recomputes what
// follows from it (derived facts, repaired links) inside its transactions, so
// data written between the preview and the apply is handled too.
type selection struct {
	derive bool   // forget: also remove facts, runs and turns derived only from the selection
	agent  string // "" = every agent
	match  string // lower-case text; "" = none

	episodes map[int64]episodeRef
	facts    map[int64]bool
	skills   map[int64]bool
	runs     map[string]bool
	sme      map[int64]bool

	// Direct cutoffs, zero when unset.
	usageBefore   time.Time
	archiveBefore time.Time
	factsBefore   time.Time // retention: facts not confirmed since

	sessions      map[string]bool // archive session files
	archiveMonths map[string]bool
	paths         map[string]bool // files and directories to remove
	profiles      []string        // profile files to scrub of match
}

// episodeRef is what the archive needs to find a forgotten turn.
type episodeRef struct {
	session string
	ts      int64
	prompt  string
}

func newSelection() selection {
	return selection{
		episodes: map[int64]episodeRef{}, facts: map[int64]bool{}, skills: map[int64]bool{},
		runs: map[string]bool{}, sme: map[int64]bool{}, sessions: map[string]bool{},
		archiveMonths: map[string]bool{}, paths: map[string]bool{},
	}
}

// Forget plans the removal of what sel selects and of whatever was derived
// only from it. Nothing changes until Apply.
func Forget(ctx context.Context, d Data, sel Selector) (*Plan, error) {
	if sel.Empty() {
		return nil, fmt.Errorf("privacy: nothing selected to forget")
	}
	for _, m := range sel.ArchiveMonths {
		if _, err := time.Parse("2006-01", m); err != nil {
			return nil, fmt.Errorf("privacy: archive month %q is not YYYY-MM", m)
		}
	}
	match := lowerASCII(strings.TrimSpace(sel.Match))
	if sel.Match != "" && match == "" {
		return nil, fmt.Errorf("privacy: the text to forget is blank")
	}
	build := func(ctx context.Context) (selection, error) {
		s := newSelection()
		s.derive = true
		s.agent = sel.Agent
		s.match = match
		for _, id := range sel.Sessions {
			s.sessions[id] = true
		}
		for _, m := range sel.ArchiveMonths {
			s.archiveMonths[m] = true
		}
		for _, id := range sel.Facts {
			s.facts[id] = true
		}
		s.usageBefore = sel.Before
		s.archiveBefore = sel.Before
		if err := selectRows(ctx, d, sel, &s); err != nil {
			return s, err
		}
		if sel.Agent == "" && !sel.Before.IsZero() {
			if err := s.selectFilesBefore(d, map[Class]time.Time{
				Research: sel.Before, Screenshots: sel.Before, RelayInbox: sel.Before, EvalArtifacts: sel.Before,
			}); err != nil {
				return s, err
			}
		}
		if sel.Agent == "" {
			if err := s.selectAttachments(d, sel.Before, s.archiveMonths); err != nil {
				return s, err
			}
		}
		if s.match != "" {
			s.profiles = profilesMentioning(d, s.agent, s.match)
		}
		return s, nil
	}
	p := &Plan{Reason: ReasonForget, d: d, build: build}
	s, err := build(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.preview(ctx, &s); err != nil {
		return nil, err
	}
	p.notes(sel, &s)
	return p, nil
}

// Prune plans the removal of everything older than its class's retention.
// Facts distilled from pruned episodes are kept; their provenance records
// the removed sources.
func Prune(ctx context.Context, d Data, r Retention) (*Plan, error) {
	build := func(ctx context.Context) (selection, error) {
		s := newSelection()
		now := d.now()
		cutoff := func(c Class) time.Time {
			if days := r[c]; days > 0 {
				return now.Add(-time.Duration(days) * 24 * time.Hour)
			}
			return time.Time{}
		}
		if t := cutoff(Episodes); !t.IsZero() {
			if err := withDB(d.path(episodesDB), true, func(db *sql.DB) error {
				return selectEpisodesWhere(ctx, db, s.episodes, "ts < ?", t.Unix())
			}); err != nil {
				return s, err
			}
		}
		s.factsBefore = cutoff(Facts)
		s.archiveBefore = cutoff(Archive)
		s.usageBefore = cutoff(Usage)
		files := map[Class]time.Time{}
		for _, c := range []Class{Research, Screenshots, RelayInbox, EvalArtifacts} {
			if t := cutoff(c); !t.IsZero() {
				files[c] = t
			}
		}
		if err := s.selectFilesBefore(d, files); err != nil {
			return s, err
		}
		if err := s.selectAttachments(d, s.archiveBefore, nil); err != nil {
			return s, err
		}
		if !s.factsBefore.IsZero() {
			if err := withDB(d.path(factsDB), true, func(db *sql.DB) error {
				return selectStaleFacts(ctx, db, s.facts, s.factsBefore)
			}); err != nil {
				return s, err
			}
		}
		return s, nil
	}
	p := &Plan{Reason: ReasonRetention, d: d, build: build}
	s, err := build(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.preview(ctx, &s); err != nil {
		return nil, err
	}
	return p, nil
}

// withDB opens a store (read-only for planning) and runs fn; a store that
// does not exist yet is skipped.
func withDB(path string, readOnly bool, fn func(*sql.DB) error) error {
	open := openStore
	if readOnly {
		open = openReadOnly
	}
	db, err := open(path)
	if missing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(db)
}

// selectRows adds the rows sel picks directly to s: episodes, facts, SME
// sections, skills and runs, and the run of every selected episode.
func selectRows(ctx context.Context, d Data, sel Selector, s *selection) error {
	with := agentScope(sel.Agent)
	err := withDB(d.path(episodesDB), true, func(db *sql.DB) error { return selectEpisodeRows(ctx, db, sel, s) })
	if err != nil {
		return err
	}
	err = withDB(d.path(factsDB), true, func(db *sql.DB) error { return selectFactRows(ctx, db, sel, s) })
	if err != nil {
		return err
	}
	err = withDB(d.path(skillsDB), true, func(db *sql.DB) error {
		if len(sel.Skills) > 0 {
			c, a := with("name IN ("+inClause(len(sel.Skills))+")", anys(sel.Skills)...)
			if err := selectIDs(ctx, db, `SELECT id FROM skills WHERE `+c, a, s.skills); err != nil {
				return err
			}
		}
		if s.match != "" {
			c, a := with(`instr(lower(name || char(10) || description || char(10) || recipe), ?) > 0`, s.match)
			if err := selectIDs(ctx, db, `SELECT id FROM skills WHERE `+c, a, s.skills); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = withDB(d.path(runsDB), true, func(db *sql.DB) error {
		var conds []string
		var args []any
		if len(sel.Sessions) > 0 {
			conds = append(conds, "session_id IN ("+inClause(len(sel.Sessions))+")")
			args = append(args, anys(sel.Sessions)...)
		}
		if !sel.Before.IsZero() {
			conds = append(conds, "started_at < ?")
			args = append(args, sel.Before.UTC().Format(time.RFC3339Nano))
		}
		if s.match != "" {
			conds = append(conds, `instr(lower(tool_calls || char(10) || approvals), ?) > 0`)
			args = append(args, s.match)
		}
		if len(conds) == 0 {
			return nil
		}
		c, a := with(strings.Join(conds, " OR "), args...)
		return selectStrings(ctx, db, `SELECT trace_id FROM runs WHERE `+c, a, s.runs)
	})
	if err != nil {
		return err
	}
	// Runs recorded for forgotten turns go with them.
	return withDB(d.path(episodesDB), true, func(db *sql.DB) error {
		return episodeRuns(ctx, db, s.episodes, s.runs)
	})
}

// agentScope adds "this agent's rows only" to a condition, when agent is set.
func agentScope(agent string) func(cond string, args ...any) (string, []any) {
	return func(cond string, args ...any) (string, []any) {
		if agent == "" {
			return "(" + cond + ")", args
		}
		return "(" + cond + ") AND agent_id = ?", append(args, agent)
	}
}

// selectEpisodeRows adds the episodes sel picks in db to s.
func selectEpisodeRows(ctx context.Context, db queryer, sel Selector, s *selection) error {
	with := agentScope(sel.Agent)
	if len(sel.Sessions) > 0 {
		c, a := with("session_id IN ("+inClause(len(sel.Sessions))+")", anys(sel.Sessions)...)
		if err := selectEpisodesWhere(ctx, db, s.episodes, c, a...); err != nil {
			return err
		}
	}
	for _, ids := range chunks(sel.Episodes, 500) {
		c, a := with("id IN ("+inClause(len(ids))+")", anys(ids)...)
		if err := selectEpisodesWhere(ctx, db, s.episodes, c, a...); err != nil {
			return err
		}
	}
	if len(sel.Runs) > 0 && hasColumn(ctx, db, "episodes", "source_run_id") {
		for _, runs := range chunks(sel.Runs, 500) {
			c, a := with("source_run_id IN ("+inClause(len(runs))+")", anys(runs)...)
			if err := selectEpisodesWhere(ctx, db, s.episodes, c, a...); err != nil {
				return err
			}
		}
	}
	if !sel.Before.IsZero() {
		c, a := with("ts < ?", sel.Before.Unix())
		if err := selectEpisodesWhere(ctx, db, s.episodes, c, a...); err != nil {
			return err
		}
	}
	if s.match != "" {
		c, a := with(`instr(lower(prompt || char(10) || response || char(10) || coalesce(tags, '') || char(10) || coalesce(tool_calls, '')), ?) > 0`, s.match)
		if err := selectEpisodesWhere(ctx, db, s.episodes, c, a...); err != nil {
			return err
		}
	}
	return nil
}

// selectFactRows adds the facts and SME sections that hold sel's text in db
// to s.
func selectFactRows(ctx context.Context, db queryer, sel Selector, s *selection) error {
	if s.match == "" {
		return nil
	}
	with := agentScope(sel.Agent)
	c, a := with(`instr(lower(fact), ?) > 0`, s.match)
	if err := selectIDs(ctx, db, `SELECT id FROM facts WHERE `+c, a, s.facts); err != nil {
		return err
	}
	if ok, err := hasTable(ctx, db, "sme_docs"); err != nil {
		return err
	} else if ok {
		c, a := with(`instr(lower(body), ?) > 0`, s.match)
		if err := selectIDs(ctx, db, `SELECT id FROM sme_docs WHERE `+c, a, s.sme); err != nil {
			return err
		}
	}
	return nil
}

func selectEpisodesWhere(ctx context.Context, db queryer, into map[int64]episodeRef, cond string, args ...any) error {
	rows, err := db.QueryContext(ctx, `SELECT id, coalesce(session_id, ''), ts, prompt FROM episodes WHERE `+cond, args...)
	if err != nil {
		return fmt.Errorf("privacy: episodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ref episodeRef
		if err := rows.Scan(&id, &ref.session, &ref.ts, &ref.prompt); err != nil {
			return err
		}
		into[id] = ref
	}
	return rows.Err()
}

// episodeRuns adds the run of every selected episode.
func episodeRuns(ctx context.Context, db queryer, eps map[int64]episodeRef, into map[string]bool) error {
	if !hasColumn(ctx, db, "episodes", "source_run_id") {
		return nil
	}
	ids := sortedIDs(eps)
	for _, chunk := range chunks(ids, 500) {
		rows, err := db.QueryContext(ctx, `SELECT source_run_id FROM episodes WHERE source_run_id IS NOT NULL AND source_run_id <> '' AND id IN (`+inClause(len(chunk))+`)`, anys(chunk)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var run string
			if err := rows.Scan(&run); err != nil {
				rows.Close()
				return err
			}
			into[run] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func selectIDs(ctx context.Context, db queryer, query string, args []any, into map[int64]bool) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("privacy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		into[id] = true
	}
	return rows.Err()
}

func selectStrings(ctx context.Context, db queryer, query string, args []any, into map[string]bool) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("privacy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		into[v] = true
	}
	return rows.Err()
}

// selectStaleFacts picks facts not confirmed since cutoff, sparing pinned and
// protected ones.
func selectStaleFacts(ctx context.Context, db queryer, into map[int64]bool, cutoff time.Time) error {
	q := `SELECT id FROM facts WHERE last_confirmed < ?`
	if ok, err := hasTable(ctx, db, "fact_signals"); err != nil {
		return err
	} else if ok {
		q += ` AND id NOT IN (SELECT fact_id FROM fact_signals WHERE pinned = 1 OR protected = 1)`
	}
	return selectIDs(ctx, db, q, []any{cutoff.Unix()}, into)
}

func sortedIDs[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// profilesMentioning lists synthesized user-profile files with a line that
// contains match.
func profilesMentioning(d Data, agent, match string) []string {
	pattern := "profile.*.md"
	if agent != "" {
		pattern = "profile." + agent + ".md"
	}
	files, _ := filepath.Glob(filepath.Join(d.DataDir, pattern))
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err == nil && containsFold(string(b), match) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
