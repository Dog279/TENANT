package semantic

import "context"

// UpdateEmbedding rewrites one fact's stored vector + embedder id, leaving the
// claim text untouched. Used to re-embed after switching embed models. See
// `tenant memory reembed`.
func (s *Store) UpdateEmbedding(ctx context.Context, id int64, vec []float32, embedderID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE facts SET embedding = ?, embedder_id = ? WHERE id = ?`,
		encodeEmbedding(vec), embedderID, id)
	return err
}
