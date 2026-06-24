package skills

import (
	"context"
	"encoding/json"
	"testing"
)

// TestBlobRoundTrip: a skill written via Upsert stores a float32 BLOB and is
// retrieved + ranked by meaning through it. We blank the legacy JSON column
// afterward to PROVE the read path is using the BLOB, not the JSON fallback.
func TestBlobRoundTrip(t *testing.T) {
	s := mk(t)
	ctx := context.Background()

	idX, err := s.Upsert(ctx, &Skill{AgentID: "a", Name: "x", Description: "x", Recipe: "r", Embedding: []float32{1, 0, 0}})
	if err != nil {
		t.Fatalf("upsert x: %v", err)
	}
	if _, err := s.Upsert(ctx, &Skill{AgentID: "a", Name: "y", Description: "y", Recipe: "r", Embedding: []float32{0, 1, 0}}); err != nil {
		t.Fatalf("upsert y: %v", err)
	}

	// Confirm the BLOB column was actually populated for x.
	var blobLen int
	if err := s.db.QueryRowContext(ctx, `SELECT length(embedding_blob) FROM skills WHERE id=?`, idX).Scan(&blobLen); err != nil {
		t.Fatalf("blob length query: %v", err)
	}
	if blobLen != 3*4 {
		t.Fatalf("embedding_blob length = %d bytes, want 12 (3 float32)", blobLen)
	}

	// Null out the legacy JSON for BOTH rows so the ONLY readable vector is the BLOB.
	if _, err := s.db.ExecContext(ctx, `UPDATE skills SET embedding=NULL`); err != nil {
		t.Fatalf("null json: %v", err)
	}

	got, err := s.Get(ctx, idX)
	if err != nil {
		t.Fatalf("get x: %v", err)
	}
	if len(got.Embedding) != 3 || got.Embedding[0] != 1 {
		t.Fatalf("decoded-from-blob embedding = %v, want [1 0 0]", got.Embedding)
	}

	hits, err := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{0.9, 0.1, 0}, K: 2})
	if err != nil || len(hits) != 2 {
		t.Fatalf("search via blob: %d hits err=%v", len(hits), err)
	}
	if hits[0].Skill.Name != "x" {
		t.Fatalf("expected x ranked first via blob, got %s", hits[0].Skill.Name)
	}
}

// TestLegacyJSONOnlyRowStillSearchable simulates a PRE-TEN-283 skills.db: a row
// with only the legacy JSON `embedding` column populated and `embedding_blob`
// NULL. It must still decode + rank correctly with NO migration step.
func TestLegacyJSONOnlyRowStillSearchable(t *testing.T) {
	s := mk(t)
	ctx := context.Background()

	// Insert directly, bypassing Upsert, to mimic an old binary that never
	// wrote the BLOB: embedding_blob stays NULL, only JSON is set.
	jx, _ := json.Marshal([]float32{1, 0, 0})
	jy, _ := json.Marshal([]float32{0, 1, 0})
	insLegacy := func(name, embJSON string) {
		_, err := s.db.ExecContext(ctx, `
            INSERT INTO skills (agent_id,name,description,recipe,status,enabled,success_count,embedder_id,embedding,created_at)
            VALUES ('a',?,?,?,'live',1,0,'old/1',?,0)`, name, name, "r", embJSON)
		if err != nil {
			t.Fatalf("legacy insert %s: %v", name, err)
		}
	}
	insLegacy("x", string(jx))
	insLegacy("y", string(jy))

	// Sanity: the blob really is NULL for these legacy rows.
	var nullBlobs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skills WHERE embedding_blob IS NULL`).Scan(&nullBlobs); err != nil {
		t.Fatalf("count null blobs: %v", err)
	}
	if nullBlobs != 2 {
		t.Fatalf("expected 2 blob-NULL legacy rows, got %d", nullBlobs)
	}

	// Decoded from the JSON fallback.
	got, err := s.GetByName(ctx, "a", "x")
	if err != nil || got == nil {
		t.Fatalf("get legacy x: %v", err)
	}
	if len(got.Embedding) != 3 || got.Embedding[0] != 1 {
		t.Fatalf("legacy JSON-decoded embedding = %v, want [1 0 0]", got.Embedding)
	}

	hits, err := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{0.9, 0.1, 0}, K: 2})
	if err != nil || len(hits) != 2 {
		t.Fatalf("search legacy: %d hits err=%v", len(hits), err)
	}
	if hits[0].Skill.Name != "x" {
		t.Fatalf("expected x first from legacy JSON, got %s", hits[0].Skill.Name)
	}
}

// TestUpdateEmbeddingBackfillsBlob mirrors what `tenant memory reembed` does:
// it calls UpdateEmbedding on a legacy JSON-only row, which must populate the
// BLOB. Search must keep working, now driven by the BLOB.
func TestUpdateEmbeddingBackfillsBlob(t *testing.T) {
	s := mk(t)
	ctx := context.Background()

	// Legacy JSON-only row (blob NULL).
	jx, _ := json.Marshal([]float32{1, 0, 0})
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO skills (agent_id,name,description,recipe,status,enabled,success_count,embedder_id,embedding,created_at)
        VALUES ('a','x','x','r','live',1,0,'old/1',?,0)`, string(jx)); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM skills WHERE name='x'`).Scan(&id); err != nil {
		t.Fatalf("id lookup: %v", err)
	}

	// BloblessIDs should report it.
	bl, err := s.BloblessIDs(ctx)
	if err != nil || len(bl) != 1 || bl[0] != id {
		t.Fatalf("BloblessIDs = %v err=%v, want [%d]", bl, err, id)
	}

	// Reembed (new dimension to also prove dim recovery): 4d vector.
	if err := s.UpdateEmbedding(ctx, id, []float32{0, 0, 1, 0}, "new/2"); err != nil {
		t.Fatalf("UpdateEmbedding: %v", err)
	}

	// Blob now present; no rows blobless.
	bl2, _ := s.BloblessIDs(ctx)
	if len(bl2) != 0 {
		t.Fatalf("expected 0 blobless rows after reembed, got %v", bl2)
	}

	// Null out JSON to prove the read now uses the BLOB.
	if _, err := s.db.ExecContext(ctx, `UPDATE skills SET embedding=NULL WHERE id=?`, id); err != nil {
		t.Fatalf("null json: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Embedding) != 4 || got.Embedding[2] != 1 || got.EmbedderID != "new/2" {
		t.Fatalf("post-reembed embedding=%v embedder=%s, want [0 0 1 0] new/2", got.Embedding, got.EmbedderID)
	}
	hits, err := s.Search(ctx, Query{AgentID: "a", Embedding: []float32{0, 0, 1, 0}, K: 1})
	if err != nil || len(hits) != 1 || hits[0].Skill.Name != "x" {
		t.Fatalf("search after reembed: hits=%d err=%v", len(hits), err)
	}
}

// TestListAllSeesEveryStatus confirms the reembed maintenance lister returns
// disabled + tombstoned rows that the retrieval-facing List hides.
func TestListAllSeesEveryStatus(t *testing.T) {
	s := mk(t)
	ctx := context.Background()
	live, _ := s.Upsert(ctx, &Skill{AgentID: "a", Name: "live", Description: "d", Recipe: "r", Embedding: []float32{1, 0}})
	tomb, _ := s.Upsert(ctx, &Skill{AgentID: "a", Name: "tomb", Description: "d", Recipe: "r", Embedding: []float32{0, 1}})
	if err := s.Tombstone(ctx, tomb); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	all, err := s.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListAll returned %d, want 2 (incl. tombstoned)", len(all))
	}
	// List (retrieval view) hides the tombstoned one.
	visible, _ := s.List(ctx, ListFilter{AgentID: "a"})
	if len(visible) != 1 || visible[0].ID != live {
		t.Fatalf("List should show only the live row, got %d", len(visible))
	}
}
