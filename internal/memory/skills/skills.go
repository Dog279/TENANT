// Package skills implements T4 of Tenant's memory architecture: the
// procedural / skill-library layer (Voyager / Hermes pattern). A skill
// is a named, reusable RECIPE — a short procedure the agent can follow
// using its existing tools — with an intent embedding so the most
// relevant skills can be retrieved per task and injected into the
// prompt. Skills are created two ways: manually (a human or the agent
// saves one) and by a background induction job that mines repeated
// successful tool sequences out of episodes (those land as `proposed`
// for human review before going live).
//
// Storage: its own SQLite file, pure-Go (modernc), brute-force cosine
// over the small set of live skills — same transparency/simplicity
// choice as the wiki tier. No FTS vtable, no decay (skills earn their
// keep via success_count, not freshness).
package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tenant/internal/memory/cosine"

	_ "modernc.org/sqlite"
)

// Status values for a skill's lifecycle.
const (
	StatusLive       = "live"       // active, eligible for retrieval
	StatusProposed   = "proposed"   // induced; awaiting human accept/reject
	StatusTombstoned = "tombstoned" // soft-deleted
)

// Skill is one reusable procedure.
type Skill struct {
	ID           int64
	AgentID      string
	Name         string
	Description  string // one line: what this skill is for (drives retrieval)
	Recipe       string // the steps to follow
	Status       string
	Enabled      bool
	SuccessCount int
	EmbedderID   string
	Embedding    []float32
	CreatedAt    time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS skills (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id      TEXT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL,
    recipe        TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'live',
    enabled       INTEGER NOT NULL DEFAULT 1,
    success_count INTEGER NOT NULL DEFAULT 0,
    embedder_id   TEXT,
    embedding     TEXT,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_skills_agent ON skills(agent_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skills_name ON skills(agent_id, name);

-- skill_history: append-only audit log of skill mutations. Each Upsert
-- on a PRE-EXISTING skill snapshots the prior row's (description,
-- recipe, status) into here BEFORE overwriting — so /skills history,
-- /skills diff, and /skills revert can answer "what did this look like
-- before the last change?". A fresh skill (first insert) has no
-- history entry; the current row IS the v1.
--
-- prior_* fields are the OLD values (the ones that were just replaced).
-- change_source tags what kind of change made it: "operator" (manual
-- /skills add or skill_save tool call), "induction" (background job
-- proposed it), "revert" (operator restored a prior version), "seed"
-- (bulk install of a starter bundle). Surfaces "who did this" in
-- /skills history without us needing a separate audit log.
CREATE TABLE IF NOT EXISTS skill_history (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    skill_id           INTEGER NOT NULL,
    agent_id           TEXT NOT NULL,
    name               TEXT NOT NULL,
    version            INTEGER NOT NULL,
    prior_description  TEXT NOT NULL,
    prior_recipe       TEXT NOT NULL,
    prior_status       TEXT NOT NULL,
    change_source      TEXT NOT NULL DEFAULT 'operator',
    changed_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_skill_history_skill ON skill_history(skill_id, version DESC);
CREATE INDEX IF NOT EXISTS idx_skill_history_lookup ON skill_history(agent_id, name, version DESC);`

// Store is the T4 skill library.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the skills DB. ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("skills: open: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("skills: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying *sql.DB. Use sparingly — most callers should
// use the typed methods. Required for `tenant doctor` to run PRAGMA
// integrity_check against the file.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Upsert inserts a new skill or replaces an existing one of the same
// (agent, name). Returns the row id. When the skill already exists, the
// PRIOR (description, recipe, status) snapshot is appended to
// skill_history before the update — so /skills history / diff / revert
// can answer "what did this look like before the last change?". The
// snapshot + the update run in ONE transaction; a failed update leaves
// history untouched.
//
// changeSource (free-text) tags what kind of change made it. The empty
// string defaults to "operator". Pass "induction" / "revert" / "seed"
// from the relevant call sites; they show up in /skills history.
func (s *Store) Upsert(ctx context.Context, sk *Skill) (int64, error) {
	return s.UpsertWithSource(ctx, sk, "operator")
}

// UpsertWithSource is Upsert + an explicit changeSource tag. Most callers
// should stick with Upsert; bulk-installers (gstack seeds) and the
// induction job pass "seed" / "induction" so the history shows the lineage.
func (s *Store) UpsertWithSource(ctx context.Context, sk *Skill, changeSource string) (int64, error) {
	if strings.TrimSpace(sk.Name) == "" || strings.TrimSpace(sk.Description) == "" {
		return 0, fmt.Errorf("skills: name and description required")
	}
	if sk.Status == "" {
		sk.Status = StatusLive
	}
	if sk.CreatedAt.IsZero() {
		sk.CreatedAt = time.Now().UTC()
	}
	if strings.TrimSpace(changeSource) == "" {
		changeSource = "operator"
	}

	// Wrap the snapshot + update in a single tx so a crash mid-Upsert can't
	// leave a "phantom" history row pointing at an update that never
	// actually replaced anything.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("skills: begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit

	// Snapshot the prior row (if any) into skill_history. We read the
	// CURRENT row's id + content + the next version number. New skills
	// (no prior row) skip this step — there's nothing to record.
	var (
		priorID   int64
		priorDesc string
		priorRcp  string
		priorStat string
	)
	row := tx.QueryRowContext(ctx,
		`SELECT id, description, recipe, status FROM skills WHERE agent_id=? AND name=?`,
		sk.AgentID, sk.Name)
	if scanErr := row.Scan(&priorID, &priorDesc, &priorRcp, &priorStat); scanErr == nil {
		// Existing skill — version = (max(skill_history.version) for this skill) + 1.
		// COALESCE handles "no prior history" (first edit).
		var nextVersion int
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM skill_history WHERE skill_id=?`,
			priorID).Scan(&nextVersion)
		if _, herr := tx.ExecContext(ctx, `
            INSERT INTO skill_history
                (skill_id, agent_id, name, version, prior_description, prior_recipe, prior_status, change_source, changed_at)
            VALUES (?,?,?,?,?,?,?,?,?)`,
			priorID, sk.AgentID, sk.Name, nextVersion, priorDesc, priorRcp, priorStat, changeSource, time.Now().UTC().Unix()); herr != nil {
			return 0, fmt.Errorf("skills: history snapshot: %w", herr)
		}
	}

	emb, _ := json.Marshal(sk.Embedding)
	res, err := tx.ExecContext(ctx, `
        INSERT INTO skills (agent_id,name,description,recipe,status,enabled,success_count,embedder_id,embedding,created_at)
        VALUES (?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_id,name) DO UPDATE SET
            description=excluded.description, recipe=excluded.recipe, status=excluded.status,
            embedder_id=excluded.embedder_id, embedding=excluded.embedding`,
		sk.AgentID, sk.Name, sk.Description, sk.Recipe, sk.Status, boolToInt(sk.Enabled || sk.Status == StatusLive),
		sk.SuccessCount, sk.EmbedderID, string(emb), sk.CreatedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("skills: upsert: %w", err)
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		// Conflict-update path — look up the id.
		_ = tx.QueryRowContext(ctx, `SELECT id FROM skills WHERE agent_id=? AND name=?`, sk.AgentID, sk.Name).Scan(&id)
	}
	// Genesis audit row for a brand-new AUTO-ACCEPTED skill (TEN-152): a net-new
	// insert normally skips history ("the live row IS v1"), but an auto-accepted
	// skill went live WITHOUT manual review — record its machine origin durably so
	// `/skills history` attributes it after the fact, not just a one-off feed line.
	// Scoped to "auto-accept" only, so operator/seed/induction inserts keep the
	// existing no-history-until-edited convention.
	if priorID == 0 && changeSource == "auto-accept" {
		if _, herr := tx.ExecContext(ctx, `
            INSERT INTO skill_history
                (skill_id, agent_id, name, version, prior_description, prior_recipe, prior_status, change_source, changed_at)
            VALUES (?,?,?,?,?,?,?,?,?)`,
			id, sk.AgentID, sk.Name, 1, sk.Description, sk.Recipe, sk.Status, changeSource, time.Now().UTC().Unix()); herr != nil {
			return 0, fmt.Errorf("skills: genesis history: %w", herr)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("skills: commit: %w", err)
	}
	return id, nil
}

// Get returns a skill by id.
func (s *Store) Get(ctx context.Context, id int64) (*Skill, error) {
	return scan(s.db.QueryRowContext(ctx, selectCols+` WHERE id=?`, id))
}

// ListFilter scopes List.
type ListFilter struct {
	AgentID         string
	Status          string // "" = any non-tombstoned
	IncludeDisabled bool
}

// List returns skills matching the filter, newest first.
func (s *Store) List(ctx context.Context, f ListFilter) ([]*Skill, error) {
	q := selectCols + ` WHERE agent_id=?`
	args := []any{f.AgentID}
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	} else {
		q += ` AND status!=?`
		args = append(args, StatusTombstoned)
	}
	if !f.IncludeDisabled {
		q += ` AND enabled=1`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Skill
	for rows.Next() {
		sk, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// Hit is a ranked retrieval result.
type Hit struct {
	Skill *Skill
	Score float64
}

// Query drives Search.
type Query struct {
	AgentID   string
	Embedding []float32
	Keywords  []string
	K         int
}

// Search returns the top-K enabled, live skills by cosine similarity to
// the query embedding, with a small lexical boost. Brute force — fine
// at skill-library scale.
func (s *Store) Search(ctx context.Context, q Query) ([]Hit, error) {
	if q.K <= 0 {
		q.K = 3
	}
	all, err := s.List(ctx, ListFilter{AgentID: q.AgentID, Status: StatusLive})
	if err != nil {
		return nil, err
	}
	kw := strings.ToLower(strings.Join(q.Keywords, " "))
	hits := make([]Hit, 0, len(all))
	for _, sk := range all {
		score := cosine.Similarity(q.Embedding, sk.Embedding)
		if kw != "" && (strings.Contains(strings.ToLower(sk.Name), kw) || strings.Contains(strings.ToLower(sk.Description), kw)) {
			score += 0.05
		}
		hits = append(hits, Hit{Skill: sk, Score: score})
	}
	// simple selection sort for top-K (small N)
	for i := 0; i < len(hits) && i < q.K; i++ {
		max := i
		for j := i + 1; j < len(hits); j++ {
			if hits[j].Score > hits[max].Score {
				max = j
			}
		}
		hits[i], hits[max] = hits[max], hits[i]
	}
	if len(hits) > q.K {
		hits = hits[:q.K]
	}
	return hits, nil
}

// SetEnabled toggles a skill by id.
func (s *Store) SetEnabled(ctx context.Context, id int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE skills SET enabled=? WHERE id=?`, boolToInt(on), id)
	return err
}

// SetEnabledByName toggles by (agent,name); returns whether it matched.
func (s *Store) SetEnabledByName(ctx context.Context, agentID, name string, on bool) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE skills SET enabled=? WHERE agent_id=? AND name=?`, boolToInt(on), agentID, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Accept promotes a proposed skill to live+enabled.
func (s *Store) Accept(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE skills SET status=?, enabled=1 WHERE id=?`, StatusLive, id)
	return err
}

// Tombstone soft-deletes a skill.
func (s *Store) Tombstone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE skills SET status=?, enabled=0 WHERE id=?`, StatusTombstoned, id)
	return err
}

// IncSuccess bumps a skill's success counter (used after a turn that
// followed it succeeded).
func (s *Store) IncSuccess(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE skills SET success_count=success_count+1 WHERE id=?`, id)
	return err
}

// Count returns live skills for an agent.
func (s *Store) Count(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skills WHERE agent_id=? AND status=?`, agentID, StatusLive).Scan(&n)
	return n, err
}

// --- internals ---

const selectCols = `SELECT id,agent_id,name,description,recipe,status,enabled,success_count,embedder_id,embedding,created_at FROM skills`

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (*Skill, error) {
	var sk Skill
	var enabled, created int64
	var embJSON, embID sql.NullString
	if err := row.Scan(&sk.ID, &sk.AgentID, &sk.Name, &sk.Description, &sk.Recipe, &sk.Status,
		&enabled, &sk.SuccessCount, &embID, &embJSON, &created); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("skills: not found")
		}
		return nil, err
	}
	sk.Enabled = enabled != 0
	sk.EmbedderID = embID.String
	sk.CreatedAt = time.Unix(created, 0).UTC()
	if embJSON.Valid && embJSON.String != "" {
		_ = json.Unmarshal([]byte(embJSON.String), &sk.Embedding)
	}
	return &sk, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- history (Option A: audit trail for skill edits) -------------------

// HistoryEntry is one prior snapshot of a skill. Returned by History()
// most-recent-first. version=1 is the OLDEST recorded edit; the highest
// version is the most recent edit. The CURRENT live skill is NOT in
// history — it's the row in the `skills` table; History() entries are
// the predecessors.
type HistoryEntry struct {
	Version          int
	PriorDescription string
	PriorRecipe      string
	PriorStatus      string
	ChangeSource     string    // "operator" | "induction" | "revert" | "seed"
	ChangedAt        time.Time // when the OVERWRITE happened
}

// History returns every snapshot of (agent, name), newest first. Returns
// nil + nil when there is no history (a fresh skill that's never been
// edited, or an unknown name).
func (s *Store) History(ctx context.Context, agentID, name string) ([]HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT version, prior_description, prior_recipe, prior_status, change_source, changed_at
        FROM skill_history
        WHERE agent_id=? AND name=?
        ORDER BY version DESC`, agentID, name)
	if err != nil {
		return nil, fmt.Errorf("skills: history query: %w", err)
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		var (
			h  HistoryEntry
			ts int64
		)
		if err := rows.Scan(&h.Version, &h.PriorDescription, &h.PriorRecipe, &h.PriorStatus, &h.ChangeSource, &ts); err != nil {
			return nil, fmt.Errorf("skills: history scan: %w", err)
		}
		h.ChangedAt = time.Unix(ts, 0).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetHistoryEntry fetches one specific prior version (useful for /skills
// diff against a particular older version, or as the source-of-truth a
// /skills revert reads from). Returns (nil, nil) when not found.
func (s *Store) GetHistoryEntry(ctx context.Context, agentID, name string, version int) (*HistoryEntry, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT version, prior_description, prior_recipe, prior_status, change_source, changed_at
        FROM skill_history
        WHERE agent_id=? AND name=? AND version=?`, agentID, name, version)
	var (
		h  HistoryEntry
		ts int64
	)
	if err := row.Scan(&h.Version, &h.PriorDescription, &h.PriorRecipe, &h.PriorStatus, &h.ChangeSource, &ts); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("skills: get history: %w", err)
	}
	h.ChangedAt = time.Unix(ts, 0).UTC()
	return &h, nil
}

// RevertTo restores a skill's (description, recipe, status) to the
// snapshot at the given version. The CURRENT state gets snapshotted into
// history (as a fresh row with change_source="revert") before being
// overwritten — so reverting is itself reversible. Embedding is NOT
// touched here; if the description changed, the caller should re-embed
// (via the wire-level AddSkill path).
//
// Returns the post-revert Skill so callers can re-embed against the new
// description. Errors if the version doesn't exist.
func (s *Store) RevertTo(ctx context.Context, agentID, name string, version int) (*Skill, error) {
	entry, err := s.GetHistoryEntry(ctx, agentID, name, version)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("skills: no history entry for %q version %d", name, version)
	}
	// Load the CURRENT row so we preserve the bits we don't restore
	// (embedder_id, success_count, agent_id, created_at).
	cur, err := s.GetByName(ctx, agentID, name)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("skills: cannot revert %q — current row is missing", name)
	}
	cur.Description = entry.PriorDescription
	cur.Recipe = entry.PriorRecipe
	cur.Status = entry.PriorStatus
	if _, err := s.UpsertWithSource(ctx, cur, "revert"); err != nil {
		return nil, err
	}
	return cur, nil
}

// GetByName looks up the live row for (agent, name). Returns (nil, nil)
// when not found (a hit-by-name helper; the existing Get() is by id).
func (s *Store) GetByName(ctx context.Context, agentID, name string) (*Skill, error) {
	sk, err := scan(s.db.QueryRowContext(ctx, selectCols+` WHERE agent_id=? AND name=?`, agentID, name))
	if err != nil && err.Error() == "skills: not found" {
		return nil, nil
	}
	return sk, err
}
