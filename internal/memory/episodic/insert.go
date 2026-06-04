package episodic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Insert writes an Episode to the store. Returns the assigned ID.
// Required fields: AgentID, Prompt, Response, EmbedderID, Embedding.
// Visibility defaults to "private" if empty. Timestamp defaults to
// now if zero. The FTS5 index updates via trigger; no extra work
// for callers.
func (s *Store) Insert(ctx context.Context, ep *Episode) (int64, error) {
	if ep == nil {
		return 0, errors.New("episodic: Insert nil episode")
	}
	if ep.AgentID == "" {
		return 0, errors.New("episodic: Insert requires AgentID")
	}
	if ep.EmbedderID == "" {
		return 0, errors.New("episodic: Insert requires EmbedderID")
	}
	if len(ep.Embedding) == 0 {
		return 0, errors.New("episodic: Insert requires non-empty Embedding")
	}
	if ep.Visibility == "" {
		ep.Visibility = VisibilityPrivate
	}
	if ep.Timestamp.IsZero() {
		ep.Timestamp = time.Now().UTC()
	}

	toolCallsJSON, err := marshalToolCalls(ep.ToolCalls)
	if err != nil {
		return 0, err
	}
	tagsJSON, err := marshalTags(ep.Tags)
	if err != nil {
		return 0, err
	}

	res, err := s.db.ExecContext(ctx, `
        INSERT INTO episodes
            (agent_id, visibility, session_id, ts, prompt, response,
             tool_calls, outcome, user_feedback, tags, embedder_id, embedding, tombstoned)
        VALUES
            (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		ep.AgentID, ep.Visibility, nullString(ep.SessionID), ep.Timestamp.UTC().Unix(),
		ep.Prompt, ep.Response,
		nullString(toolCallsJSON), nullString(ep.Outcome), nullString(ep.UserFeedback),
		nullString(tagsJSON), ep.EmbedderID, encodeEmbedding(ep.Embedding),
		boolToInt(ep.Tombstoned),
	)
	if err != nil {
		return 0, fmt.Errorf("episodic: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("episodic: lastinsertid: %w", err)
	}
	ep.ID = id
	return id, nil
}

// Get fetches an episode by ID. Returns ErrNotFound if no row matches.
// Tombstoned episodes are returned (Get is the audit path); filter
// callers do their own check.
func (s *Store) Get(ctx context.Context, id int64) (*Episode, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, agent_id, visibility, session_id, ts, prompt, response,
               tool_calls, outcome, user_feedback, tags, embedder_id, embedding, tombstoned
        FROM episodes WHERE id = ?
    `, id)
	return scanEpisode(row)
}

// Tombstone marks an episode as removed from retrieval without
// deleting the row. The episode still exists for audit but Search
// will skip it.
func (s *Store) Tombstone(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE episodes SET tombstoned = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("episodic: tombstone: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	return nil
}

// Count returns the total episode count for diagnostics. Tombstoned
// rows ARE counted — pass tombstoned=false to exclude.
func (s *Store) Count(ctx context.Context, includeTombstoned bool) (int, error) {
	q := `SELECT COUNT(*) FROM episodes`
	if !includeTombstoned {
		q += ` WHERE tombstoned = 0`
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("episodic: count: %w", err)
	}
	return n, nil
}

// --- helpers ---

// rowScanner is the minimal interface satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEpisode(row rowScanner) (*Episode, error) {
	var (
		ep             Episode
		sessionID      sql.NullString
		toolCallsJSON  sql.NullString
		outcome        sql.NullString
		userFeedback   sql.NullString
		tagsJSON       sql.NullString
		embeddingBytes []byte
		tsUnix         int64
		tombstonedInt  int
	)
	err := row.Scan(
		&ep.ID, &ep.AgentID, &ep.Visibility, &sessionID, &tsUnix,
		&ep.Prompt, &ep.Response,
		&toolCallsJSON, &outcome, &userFeedback, &tagsJSON,
		&ep.EmbedderID, &embeddingBytes, &tombstonedInt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("episodic: scan: %w", err)
	}
	ep.SessionID = sessionID.String
	ep.Outcome = outcome.String
	ep.UserFeedback = userFeedback.String
	ep.Timestamp = time.Unix(tsUnix, 0).UTC()
	ep.Tombstoned = tombstonedInt != 0

	// Tolerant decode for the JSON-blob fields. A single corrupt row would
	// otherwise poison the WHOLE hydrate path (retrieval, assemble, the next
	// turn) — and the episode's prompt/response are independently useful even
	// without the metadata. Drop the bad field, keep the episode. The
	// canonical trigger seen live was a malformed tool_calls string ("garbage"
	// instead of valid JSON) from an earlier model-output bug, surfacing
	// months later as "agent: assemble … invalid character 'g'."
	if toolCallsJSON.Valid && toolCallsJSON.String != "" {
		if err := json.Unmarshal([]byte(toolCallsJSON.String), &ep.ToolCalls); err != nil {
			ep.ToolCalls = nil // corrupt row — episode still usable without tool meta
		}
	}
	if tagsJSON.Valid && tagsJSON.String != "" {
		if err := json.Unmarshal([]byte(tagsJSON.String), &ep.Tags); err != nil {
			ep.Tags = nil
		}
	}
	if len(embeddingBytes) > 0 {
		ep.Embedding, err = decodeEmbedding(embeddingBytes)
		if err != nil {
			return nil, err
		}
	}
	return &ep, nil
}

func marshalToolCalls(tc []ToolCallRef) (string, error) {
	if len(tc) == 0 {
		return "", nil
	}
	b, err := json.Marshal(tc)
	if err != nil {
		return "", fmt.Errorf("episodic: marshal tool_calls: %w", err)
	}
	return string(b), nil
}

func marshalTags(tags []string) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("episodic: marshal tags: %w", err)
	}
	return string(b), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
