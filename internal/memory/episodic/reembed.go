package episodic

import "context"

// UpdateEmbedding rewrites one episode's stored vector + embedder id, leaving
// the text untouched. Used to re-embed after switching embed models (a new
// embedder's dimension makes old vectors unsearchable — cosine zeroes on a
// length mismatch). See `tenant memory reembed`.
func (s *Store) UpdateEmbedding(ctx context.Context, id int64, vec []float32, embedderID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET embedding = ?, embedder_id = ? WHERE id = ?`,
		encodeEmbedding(vec), embedderID, id)
	return err
}
