package wiki

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hung takes the request and answers only when the caller gives up: an
// embedder whose runner is stuck (TEN-555).
type hung struct{}

func (hung) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// One embedding request is given up after its limit, whatever the caller's
// context allows: a reindex and a search both come back, saying why.
func TestHungEmbedderIsGivenUpOn(t *testing.T) {
	prevFile, prevQuery := fileEmbedTimeout, queryEmbedTimeout
	fileEmbedTimeout, queryEmbedTimeout = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { fileEmbedTimeout, queryEmbedTimeout = prevFile, prevQuery })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\nsome words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ix, err := New(dir, filepath.Join(t.TempDir(), "ix.json"), "hung/8", hung{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := ix.Reindex(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not answer within") {
			t.Fatalf("reindex with a hung embedder: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reindex waited on a hung embedder past its limit")
	}
	if _, err := ix.Search(context.Background(), "words", 3); err == nil {
		t.Error("a search with a hung embedder returned no error")
	}
}

// slow embeds only once it is released, and says when it was first asked.
type slow struct{ asked, release chan struct{} }

func (s slow) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case s.asked <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// ReindexBehind returns before the embedder has answered, and a reader that
// comes while it runs waits for it: it never sees the index as it was before.
func TestReindexBehindHoldsReadersUntilItIsDone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\nsome words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emb := slow{asked: make(chan struct{}, 1), release: make(chan struct{})}
	ix, err := New(dir, filepath.Join(t.TempDir(), "ix.json"), "slow/2", emb)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	ix.ReindexBehind(context.Background(), func(err error) { ended <- err }) // returns with the embedder not yet answered
	<-emb.asked

	read := make(chan error, 1)
	go func() { _, _, _, _, err := ix.Links("a.md"); read <- err }()
	select {
	case err := <-read:
		t.Fatalf("a reader did not wait for the reindex: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(emb.release)
	if err := <-ended; err != nil {
		t.Fatalf("the reindex: %v", err)
	}
	if err := <-read; err != nil {
		t.Errorf("the reader that waited did not find the note: %v", err)
	}
}

// Close ends a reindex still running behind: it does not wait on the
// embedder, and when it returns the index has let go of its file, which a
// test's (or a plugin's) clean-up may then remove.
func TestCloseEndsTheReindexBehind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\nsome words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(t.TempDir(), "ix.json")
	emb := slow{asked: make(chan struct{}, 1), release: make(chan struct{})}
	ix, err := New(dir, side, "slow/2", emb)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	ix.ReindexBehind(context.Background(), func(err error) { ended <- err })
	<-emb.asked

	closed := make(chan struct{})
	go func() { ix.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waited on an embedder that had not answered")
	}
	if err := <-ended; err == nil {
		t.Error("a reindex that was cut short reported success")
	}
	if _, err := os.Stat(side); err == nil {
		t.Error("the index's file was written by a reindex that was cut short")
	}
	ix.Close() // a second time changes nothing
	if got := ix.List(); len(got) != 0 {
		t.Errorf("after a reindex that was cut short the index lists %v", got)
	}
}
