package route

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDecisionLines(t *testing.T, path string, decisions []Decision, extraRaw ...string) {
	t.Helper()
	var b strings.Builder
	for _, d := range decisions {
		line, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	for _, raw := range extraRaw {
		b.WriteString(raw + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPruneDecisionLogDropsOnlyOldRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DecisionLogName)
	now := time.Now().UTC()
	old := Decision{ID: "old", Timestamp: now.Add(-100 * 24 * time.Hour), Mode: ModeShadow}
	fresh := Decision{ID: "fresh", Timestamp: now.Add(-time.Hour), Mode: ModeShadow}
	writeDecisionLines(t, path, []Decision{old, fresh}, `{not json`)

	kept, dropped, err := PruneDecisionLog(path, now.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneDecisionLog: %v", err)
	}
	// Malformed rows are retained by design (audit evidence).
	if kept != 2 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want kept=2 dropped=1", kept, dropped)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if strings.Contains(content, `"old"`) {
		t.Fatalf("old row survived prune:\n%s", content)
	}
	if !strings.Contains(content, `"fresh"`) || !strings.Contains(content, "{not json") {
		t.Fatalf("fresh or malformed row lost:\n%s", content)
	}
}

func TestPruneDecisionLogMissingFileIsNoop(t *testing.T) {
	kept, dropped, err := PruneDecisionLog(filepath.Join(t.TempDir(), "absent.jsonl"), time.Now())
	if err != nil || kept != 0 || dropped != 0 {
		t.Fatalf("missing file: kept=%d dropped=%d err=%v, want 0/0/nil", kept, dropped, err)
	}
}

func TestPruneDecisionLogNoRewriteWhenNothingDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DecisionLogName)
	fresh := Decision{ID: "fresh", Timestamp: time.Now().UTC(), Mode: ModeShadow}
	writeDecisionLines(t, path, []Decision{fresh})
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, dropped, err := PruneDecisionLog(path, time.Now().Add(-time.Hour)); err != nil || dropped != 0 {
		t.Fatalf("dropped=%d err=%v, want 0/nil", dropped, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("file rewritten although nothing was dropped")
	}
}

func TestEnginePruneAppliesRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DecisionLogName)
	now := time.Now().UTC()
	writeDecisionLines(t, path, []Decision{
		{ID: "ancient", Timestamp: now.Add(-91 * 24 * time.Hour)},
		{ID: "recent", Timestamp: now.Add(-time.Hour)},
	})
	e := NewEngine(nil, dir, 0)
	e.SetRetention(90)
	kept, dropped, err := e.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if kept != 1 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 1/1", kept, dropped)
	}
}

func TestEngineRetentionDisabledPrunesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DecisionLogName)
	writeDecisionLines(t, path, []Decision{{ID: "ancient", Timestamp: time.Now().Add(-400 * 24 * time.Hour)}})
	e := NewEngine(nil, dir, 0)
	e.SetRetention(-1)
	if _, dropped, err := e.Prune(); err != nil || dropped != 0 {
		t.Fatalf("dropped=%d err=%v, want 0/nil (retention disabled)", dropped, err)
	}
}

// TestRecordTriggersDailyPruneGate exercises the append-path gate: the first
// record prunes (lastPrune unset), a second immediate record must not.
func TestRecordTriggersDailyPruneGate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DecisionLogName)
	now := time.Now().UTC()
	writeDecisionLines(t, path, []Decision{{ID: "ancient", Timestamp: now.Add(-200 * 24 * time.Hour)}})

	e := NewEngine(nil, dir, 0)
	e.SetRetention(90)
	e.record(Decision{ID: "r1", Timestamp: now})
	// The append-path prune is deliberately asynchronous; poll briefly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw := readLogRetry(t, path, deadline)
		if !strings.Contains(string(raw), `"ancient"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("append-path prune did not run:\n%s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Re-seed an old row; within the 24h gate the next record must NOT prune.
	writeDecisionLines(t, path, []Decision{
		{ID: "ancient2", Timestamp: now.Add(-200 * 24 * time.Hour)},
		{ID: "r1", Timestamp: now},
	})
	e.record(Decision{ID: "r2", Timestamp: now})
	time.Sleep(50 * time.Millisecond) // give a (buggy) second prune time to fire
	raw := readLogRetry(t, path, time.Now().Add(2*time.Second))
	if !strings.Contains(string(raw), `"ancient2"`) {
		t.Fatal("prune ran again inside the 24h gate")
	}
}

// readLogRetry reads path, tolerating the transient failure a read hits when it
// lands exactly while a concurrent prune is swapping the log in
// (replaceDecisionLog writes a temp file then os.Rename's it over path). On
// Windows that overlap surfaces as a sharing violation ("The process cannot
// access the file because it is being used by another process"); on any OS a
// momentary miss is the rewrite in flight, not a real failure. Retry until the
// deadline so the test measures pruning, not this race. The production readers
// (decisionsForStats, the dashboard) already fall back on such an error; only
// the test asserted strictly enough to flake.
func readLogRetry(t *testing.T, path string, deadline time.Time) []byte {
	t.Helper()
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
