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
