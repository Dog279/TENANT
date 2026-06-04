//go:build livesearch

package web

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Live smoke: hit the real DDG endpoint with a realistic query and
// confirm we get parseable results back. Build-tagged so normal `go test`
// stays hermetic; run with: go test -tags livesearch -run TestLive_DDG -v
func TestLive_DDG_NvidiaStock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := NewSearcher(Config{})
	results, err := s.Search(ctx, "nvidia stock price today", 5)
	if err != nil {
		t.Fatalf("DDG search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("DDG returned ZERO results — this is the bug")
	}
	for i, r := range results {
		fmt.Printf("[%d] %s\n    %s\n    %s\n", i+1, r.Title, r.URL, r.Snippet)
	}
}
