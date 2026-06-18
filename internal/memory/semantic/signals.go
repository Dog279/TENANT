package semantic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// DefaultImportance is the neutral score a fact carries when it has no
// signals row. Anchored at the midpoint so an unscored fact ranks AND
// decays exactly as it did before fact_signals existed (additive).
const DefaultImportance = 0.5

// ProtectImportance is the strict cutoff (≈9/10) above which a fact may
// be merge-protected — but only when it is ALSO actually used (see
// MergeProtected). Deliberately high so auto-scored facts don't starve
// consolidation (design §7, review finding 3).
const ProtectImportance = 0.9

// Heat shaping. heatScore is built ONLY from the signals row so a fact
// with no row — or one never retrieved — scores exactly 0, preserving
// pre-signals ranking. (The design's source-episode engagement term is
// deferred to keep Phase 1 strictly additive.)
const (
	heatSaturation = 5.0                 // ~5 accesses ⇒ heat ≈ 0.63
	heatRecencyTau = 30 * 24 * time.Hour // access-recency decay constant
)

// Signals is the per-fact side record: importance, revealed heat, the
// pin/protect flags, and the reserved Phase-2/3 temporal + project
// columns. Obtain via GetSignals/SignalsBatch (which seed the neutral
// Importance default); the zero value is NOT the default.
type Signals struct {
	FactID       int64
	Importance   float64   // 0..1; DefaultImportance when unscored
	ConfirmCount int64     // times importance has been (re)scored
	AccessCount  int64     // retrieval hits (revealed value)
	LastAccessed time.Time // zero = never accessed
	Pinned       bool      // always-include + decay/merge immune
	Protected    bool      // merge-immune only
	ValidFrom    time.Time // Phase 2; zero = unknown
	ValidTo      time.Time // Phase 2; zero = currently true
	ProjectID    string    // Phase 3; "" = global
}

func defaultSignals(id int64) Signals {
	return Signals{FactID: id, Importance: DefaultImportance}
}

// MergeProtected reports whether a fact with these signals must be
// excluded from consolidation merging: pinned, explicitly protected, or
// high-importance AND actually used (access_count>0). Calibrated strict
// (ProtectImportance≈0.9 + actually-used) so auto-scored facts don't
// starve consolidation (design §7, review finding 3).
func MergeProtected(sig Signals) bool {
	if sig.Pinned || sig.Protected {
		return true
	}
	return sig.Importance >= ProtectImportance && sig.AccessCount > 0
}

// heatScore is the MemoryOS revealed-value signal in [0,1). Zero for a
// fact never retrieved (preserving pre-signals ranking exactly).
func heatScore(sig Signals, now time.Time) float64 {
	if sig.AccessCount <= 0 {
		return 0
	}
	h := 1 - math.Exp(-float64(sig.AccessCount)/heatSaturation)
	if !sig.LastAccessed.IsZero() {
		if age := now.Sub(sig.LastAccessed); age > 0 {
			h *= math.Exp(-float64(age) / float64(heatRecencyTau))
		}
	}
	return h
}

const signalsCols = `fact_id, importance, confirm_count, access_count, last_accessed,
	pinned, protected, valid_from, valid_to, project_id`

// GetSignals returns the signals for a fact, or the neutral defaults
// (importance=0.5) if the fact has no signals row yet. Never ErrNotFound.
func (s *Store) GetSignals(ctx context.Context, id int64) (Signals, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+signalsCols+` FROM fact_signals WHERE fact_id = ?`, id)
	sig, err := scanSignals(row)
	if errors.Is(err, ErrNotFound) {
		return defaultSignals(id), nil
	}
	return sig, err
}

// SignalsBatch loads signals for many fact IDs in one query, filling the
// neutral default for any ID that has no row. Used by the retrieval and
// consolidation paths.
func (s *Store) SignalsBatch(ctx context.Context, ids []int64) (map[int64]Signals, error) {
	out := make(map[int64]Signals, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+signalsCols+` FROM fact_signals WHERE fact_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic: signals batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		sig, serr := scanSignals(rows)
		if serr != nil {
			return nil, serr
		}
		out[sig.FactID] = sig
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic: signals batch iter: %w", err)
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			out[id] = defaultSignals(id)
		}
	}
	return out, nil
}

// UpsertSignals writes (or replaces) a fact's full signals row.
func (s *Store) UpsertSignals(ctx context.Context, sig Signals) error {
	if sig.FactID == 0 {
		return errors.New("semantic: UpsertSignals requires FactID")
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO fact_signals
            (fact_id, importance, confirm_count, access_count, last_accessed,
             pinned, protected, valid_from, valid_to, project_id)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(fact_id) DO UPDATE SET
            importance    = excluded.importance,
            confirm_count = excluded.confirm_count,
            access_count  = excluded.access_count,
            last_accessed = excluded.last_accessed,
            pinned        = excluded.pinned,
            protected     = excluded.protected,
            valid_from    = excluded.valid_from,
            valid_to      = excluded.valid_to,
            project_id    = excluded.project_id`,
		sig.FactID, clampImportance(sig.Importance), sig.ConfirmCount, sig.AccessCount,
		nullableUnix(sig.LastAccessed), boolToInt(sig.Pinned), boolToInt(sig.Protected),
		nullableUnix(sig.ValidFrom), nullableUnix(sig.ValidTo), nullString(sig.ProjectID))
	if err != nil {
		return fmt.Errorf("semantic: upsert signals: %w", err)
	}
	return nil
}

// ReinforceImportance folds a fresh importance score into a fact's
// standing importance as a confirmation-count-weighted running average,
// and bumps confirm_count. This is AGREEMENT-averaging, not a one-way
// ratchet: a later LOWER score pulls a spurious early high back down
// (review finding 4). First score (no prior row) sets importance directly.
func (s *Store) ReinforceImportance(ctx context.Context, id int64, newImportance float64) error {
	cur, err := s.GetSignals(ctx, id)
	if err != nil {
		return err
	}
	newImportance = clampImportance(newImportance)
	n := cur.ConfirmCount
	if n < 0 {
		n = 0
	}
	cur.Importance = (cur.Importance*float64(n) + newImportance) / float64(n+1)
	cur.ConfirmCount = n + 1
	return s.UpsertSignals(ctx, cur)
}

// BumpAccess increments access_count and sets last_accessed=now for each
// id (one transaction). A fact with no signals row gets one seeded at the
// neutral importance default, so a bump never changes importance/decay.
// Best-effort caller pattern: fire off the retrieval hot path.
func (s *Store) BumpAccess(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("semantic: bumpaccess tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO fact_signals (fact_id, access_count, last_accessed)
        VALUES (?, 1, ?)
        ON CONFLICT(fact_id) DO UPDATE SET
            access_count  = access_count + 1,
            last_accessed = excluded.last_accessed`)
	if err != nil {
		return fmt.Errorf("semantic: bumpaccess prep: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id, now); err != nil {
			return fmt.Errorf("semantic: bumpaccess exec: %w", err)
		}
	}
	return tx.Commit()
}

// SetPinned toggles a fact's pin (always-include + decay/merge immune).
func (s *Store) SetPinned(ctx context.Context, id int64, pinned bool) error {
	cur, err := s.GetSignals(ctx, id)
	if err != nil {
		return err
	}
	cur.Pinned = pinned
	return s.UpsertSignals(ctx, cur)
}

// SetProtected toggles a fact's merge-protection flag.
func (s *Store) SetProtected(ctx context.Context, id int64, protected bool) error {
	cur, err := s.GetSignals(ctx, id)
	if err != nil {
		return err
	}
	cur.Protected = protected
	return s.UpsertSignals(ctx, cur)
}

// PinnedFacts returns up to limit live pinned facts for agentID, highest
// importance first. The always-include sub-tier (design §9). limit<=0 → 5.
func (s *Store) PinnedFacts(ctx context.Context, agentID string, limit int) ([]*Fact, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT f.id, f.agent_id, f.visibility, f.fact, f.source_episodes, f.confidence,
               f.first_seen, f.last_confirmed, f.superseded_by, f.embedder_id, f.embedding, f.tombstoned
        FROM facts f JOIN fact_signals sg ON sg.fact_id = f.id
        WHERE sg.pinned = 1 AND f.tombstoned = 0 AND f.superseded_by IS NULL AND f.agent_id = ?
        ORDER BY sg.importance DESC, f.id DESC
        LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("semantic: pinned facts: %w", err)
	}
	defer rows.Close()
	var out []*Fact
	for rows.Next() {
		f, serr := scanFact(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic: pinned facts iter: %w", err)
	}
	return out, nil
}

// MergeProtectedStats returns (protected, live) counts for agentID: how
// many live facts are currently merge-protected vs. total live. A high
// ratio means consolidation is being starved (design §7, finding 3) — a
// doctor/telemetry signal to raise ProtectImportance.
func (s *Store) MergeProtectedStats(ctx context.Context, agentID string) (protectedN, liveN int, err error) {
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM facts WHERE agent_id = ? AND tombstoned = 0 AND superseded_by IS NULL`,
		agentID).Scan(&liveN); err != nil {
		return 0, 0, fmt.Errorf("semantic: protected stats live: %w", err)
	}
	if err = s.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM facts f JOIN fact_signals sg ON sg.fact_id = f.id
        WHERE f.agent_id = ? AND f.tombstoned = 0 AND f.superseded_by IS NULL
          AND (sg.pinned = 1 OR sg.protected = 1 OR (sg.importance >= ? AND sg.access_count > 0))`,
		agentID, ProtectImportance).Scan(&protectedN); err != nil {
		return 0, 0, fmt.Errorf("semantic: protected stats: %w", err)
	}
	return protectedN, liveN, nil
}

// DampenHeat halves access_count for the topN hottest facts of agentID
// (MemoryOS promotion-reset) so a few facts can't permanently dominate
// ranking. Only facts with access_count>1 are touched (so a single hit is
// never erased). Halving ROUNDS UP ((n+1)/2: 7→4, 3→2, 2→1) to preserve a
// little more of the revealed signal than a floor would. Runs in the
// always-on consolidation cadence. topN<=0 → no-op.
func (s *Store) DampenHeat(ctx context.Context, agentID string, topN int) error {
	if topN <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
        UPDATE fact_signals SET access_count = (access_count + 1) / 2
        WHERE fact_id IN (
            SELECT sg.fact_id FROM fact_signals sg JOIN facts f ON f.id = sg.fact_id
            WHERE f.agent_id = ? AND sg.access_count > 1
            ORDER BY sg.access_count DESC
            LIMIT ?)`, agentID, topN)
	if err != nil {
		return fmt.Errorf("semantic: dampen heat: %w", err)
	}
	return nil
}

// --- helpers ---

func scanSignals(row rowScanner) (Signals, error) {
	var (
		sig          Signals
		lastAccessed sql.NullInt64
		validFrom    sql.NullInt64
		validTo      sql.NullInt64
		projectID    sql.NullString
		pinnedInt    int
		protectedInt int
	)
	err := row.Scan(
		&sig.FactID, &sig.Importance, &sig.ConfirmCount, &sig.AccessCount,
		&lastAccessed, &pinnedInt, &protectedInt, &validFrom, &validTo, &projectID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Signals{}, ErrNotFound
		}
		return Signals{}, fmt.Errorf("semantic: scan signals: %w", err)
	}
	sig.Pinned = pinnedInt != 0
	sig.Protected = protectedInt != 0
	if lastAccessed.Valid {
		sig.LastAccessed = time.Unix(lastAccessed.Int64, 0).UTC()
	}
	if validFrom.Valid {
		sig.ValidFrom = time.Unix(validFrom.Int64, 0).UTC()
	}
	if validTo.Valid {
		sig.ValidTo = time.Unix(validTo.Int64, 0).UTC()
	}
	if projectID.Valid {
		sig.ProjectID = projectID.String
	}
	return sig, nil
}

func clampImportance(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func nullableUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Unix()
}
