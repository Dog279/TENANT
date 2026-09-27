package privacy

import (
	"context"
	"database/sql"
	"time"
)

// Session summarizes one conversation session, to find the id to forget.
type Session struct {
	ID          string
	Agent       string
	First, Last time.Time
	Turns       int
	// Opening is the session's first prompt, clipped.
	Opening string
}

// Sessions lists the most recent sessions, newest first.
func Sessions(ctx context.Context, d Data, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 20
	}
	var out []Session
	err := withDB(d.path(episodesDB), true, func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, `
            SELECT session_id, min(agent_id), min(ts), max(ts), count(*),
                   (SELECT prompt FROM episodes e2 WHERE e2.session_id = e.session_id ORDER BY ts, id LIMIT 1)
            FROM episodes e
            WHERE session_id IS NOT NULL AND session_id <> ''
            GROUP BY session_id ORDER BY max(ts) DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s Session
			var first, last int64
			var opening sql.NullString
			if err := rows.Scan(&s.ID, &s.Agent, &first, &last, &s.Turns, &opening); err != nil {
				return err
			}
			s.First, s.Last = time.Unix(first, 0), time.Unix(last, 0)
			s.Opening = clip(opening.String, 70)
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// Counts reports how many rows each store table holds, for a status view.
// Missing stores and tables are left out.
func Counts(ctx context.Context, d Data) map[string]int {
	out := map[string]int{}
	for _, t := range []struct{ db, table, key string }{
		{episodesDB, "episodes", "episodes"},
		{factsDB, "facts", "facts"},
		{skillsDB, "skills", "skills"},
		{runsDB, "runs", "runs"},
		{usageDB, "token_usage", "usage"},
	} {
		_ = withDB(d.path(t.db), true, func(db *sql.DB) error {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+t.table).Scan(&n); err == nil {
				out[t.key] = n
			}
			return nil
		})
	}
	return out
}
