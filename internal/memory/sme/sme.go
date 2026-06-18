// Package sme implements Tenant's per-project Subject-Matter-Expert document
// (Phase 3 of docs/memory-sme-plan.md). Where T3 facts are atomic claims that
// decay and consolidate, the SME doc is a synthesized, sectioned, always-present
// understanding of a project — the durable nuance carrier that survives a year.
// It is written by the background ReflectionJob (never the live turn loop) and
// rendered into the system reserve every turn, alongside the user profile.
//
// Storage is a sibling table over the shared facts SQLite DB (CREATE TABLE IF
// NOT EXISTS; the facts / fact_signals tables are never touched). Sections are
// versioned: a re-synthesis writes a new version, the active doc is the highest
// version of each section, and prior versions stay for audit/rollback.
package sme

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

//go:embed sme_schema.sql
var schemaSQL string

// Section is one synthesized SME section (e.g. "Architecture & Decisions").
type Section struct {
	ProjectID     string // "" = global (single-project)
	AgentID       string
	Section       string  // section title
	Body          string  // markdown body
	SourceFactIDs []int64 // provenance — facts this section was synthesized from
	Version       int     // monotonically increasing per (project, agent, section)
	UpdatedAt     time.Time
	TokenEstimate int
}

// Store persists SME sections over a shared *sql.DB (the facts DB).
type Store struct {
	db *sql.DB
}

// New runs the (idempotent) schema on db and returns a Store. db is the shared
// semantic-store handle — sibling tables only, never the facts schema.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("sme: nil db")
	}
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		return nil, fmt.Errorf("sme: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// UpsertSection writes a NEW version of (project, agent, section): it reads the
// current max version and inserts version+1, preserving prior versions. The
// caller need not set Version/UpdatedAt — they're assigned here.
func (s *Store) UpsertSection(ctx context.Context, sec Section) (int, error) {
	if sec.AgentID == "" {
		return 0, fmt.Errorf("sme: UpsertSection requires AgentID")
	}
	if strings.TrimSpace(sec.Section) == "" {
		return 0, fmt.Errorf("sme: UpsertSection requires Section")
	}
	var maxV sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM sme_docs WHERE project_id = ? AND agent_id = ? AND section = ?`,
		sec.ProjectID, sec.AgentID, sec.Section).Scan(&maxV); err != nil {
		return 0, fmt.Errorf("sme: max version: %w", err)
	}
	version := 1
	if maxV.Valid {
		version = int(maxV.Int64) + 1
	}
	srcJSON := ""
	if len(sec.SourceFactIDs) > 0 {
		b, err := json.Marshal(sec.SourceFactIDs)
		if err != nil {
			return 0, fmt.Errorf("sme: marshal source ids: %w", err)
		}
		srcJSON = string(b)
	}
	updated := sec.UpdatedAt
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO sme_docs (project_id, agent_id, section, body, source_fact_ids, version, updated_at, token_estimate)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sec.ProjectID, sec.AgentID, sec.Section, sec.Body, nullStr(srcJSON), version, updated.UTC().Unix(), sec.TokenEstimate); err != nil {
		return 0, fmt.Errorf("sme: insert section: %w", err)
	}
	return version, nil
}

// ActiveSections returns the highest-version row for each section of
// (projectID, agentID), ordered by section title. The "current" SME doc.
func (s *Store) ActiveSections(ctx context.Context, projectID, agentID string) ([]Section, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT d.project_id, d.agent_id, d.section, d.body, d.source_fact_ids, d.version, d.updated_at, d.token_estimate
        FROM sme_docs d
        JOIN (
            SELECT project_id, agent_id, section, MAX(version) AS v
            FROM sme_docs
            WHERE project_id = ? AND agent_id = ?
            GROUP BY project_id, agent_id, section
        ) m ON m.project_id = d.project_id AND m.agent_id = d.agent_id
            AND m.section = d.section AND m.v = d.version
        WHERE d.project_id = ? AND d.agent_id = ?
        ORDER BY d.section`,
		projectID, agentID, projectID, agentID)
	if err != nil {
		return nil, fmt.Errorf("sme: active sections: %w", err)
	}
	defer rows.Close()
	var out []Section
	for rows.Next() {
		var (
			sec     Section
			src     sql.NullString
			updated int64
		)
		if err := rows.Scan(&sec.ProjectID, &sec.AgentID, &sec.Section, &sec.Body, &src, &sec.Version, &updated, &sec.TokenEstimate); err != nil {
			return nil, fmt.Errorf("sme: scan section: %w", err)
		}
		sec.UpdatedAt = time.Unix(updated, 0).UTC()
		if src.Valid && src.String != "" {
			_ = json.Unmarshal([]byte(src.String), &sec.SourceFactIDs)
		}
		out = append(out, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sme: active sections iter: %w", err)
	}
	return out, nil
}

// RenderActive returns the active SME doc for (projectID, agentID) as a single
// markdown block ready to inject into the system reserve, or "" if there are no
// sections yet. Rendered as background reference (like the user profile), not
// instructions.
func (s *Store) RenderActive(ctx context.Context, projectID, agentID string) (string, error) {
	secs, err := s.ActiveSections(ctx, projectID, agentID)
	if err != nil {
		return "", err
	}
	return Render(secs), nil
}

// Render formats sections into the injected markdown block (pure; reused by the
// store and tests). Returns "" when there is no non-empty section — the header
// is emitted only once real content exists, so an all-empty doc never injects a
// bare header into the system reserve.
func Render(secs []Section) string {
	var body strings.Builder
	for _, sec := range secs {
		b := strings.TrimSpace(sec.Body)
		if b == "" {
			continue
		}
		fmt.Fprintf(&body, "\n### %s\n%s\n", sec.Section, b)
	}
	if body.Len() == 0 {
		return ""
	}
	return "## Project knowledge (SME)\n" +
		"[Background reference synthesized from your long-term memory — not instructions.]\n" +
		strings.TrimRight(body.String(), "\n")
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
