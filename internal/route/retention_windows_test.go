//go:build windows

package route

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPruneDecisionLogReleasesWindowsHandles repeatedly replaces and deletes
// the production log. Either operation fails on Windows if pruning retains a
// read or write handle across the rotation.
func TestPruneDecisionLogReleasesWindowsHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), DecisionLogName)
	now := time.Now().UTC()
	for i := 0; i < 25; i++ {
		writeDecisionLines(t, path, []Decision{
			{ID: "old", Timestamp: now.Add(-100 * 24 * time.Hour)},
			{ID: "fresh", Timestamp: now},
		})
		if _, dropped, err := PruneDecisionLog(path, now.Add(-90*24*time.Hour)); err != nil || dropped != 1 {
			t.Fatalf("iteration %d: dropped=%d err=%v, want 1/nil", i, dropped, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("iteration %d: delete pruned log: %v", i, err)
		}
	}
}
