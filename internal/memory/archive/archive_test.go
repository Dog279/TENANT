package archive_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/archive"
)

// A tool call with empty Arguments is valid (no-arg tool) but an empty
// json.RawMessage marshals to invalid JSON. Append must normalize it to {}
// rather than failing the whole event (was a recurring warn in the field).
func TestArchive_AppendNormalizesEmptyToolArgs(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	ts := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	err := w.Append(archive.Event{
		Timestamp: ts, AgentID: "main", SessionID: "s", Role: "assistant",
		ToolCalls: []archive.ToolCall{
			{ID: "1", Name: "now"},                                      // nil Arguments
			{ID: "2", Name: "x", Arguments: json.RawMessage("  ")},      // blank Arguments
			{ID: "3", Name: "y", Arguments: json.RawMessage(`{"a":1}`)}, // real args preserved
		},
	})
	if err != nil {
		t.Fatalf("Append with empty tool args should not fail: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "archive", "2026-05", "s.jsonl"))
	line := string(b)
	if strings.Count(line, `"arguments":{}`) != 2 {
		t.Fatalf("empty args not normalized to {}: %s", line)
	}
	if !strings.Contains(line, `"arguments":{"a":1}`) {
		t.Fatalf("real args not preserved: %s", line)
	}
}

func TestArchive_AppendCreatesMonthlyDir(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)

	ts := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	if err := w.Append(archive.Event{
		Timestamp: ts, AgentID: "main", SessionID: "sess1", Role: "user", Content: "hi",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	want := filepath.Join(dir, "archive", "2026-05", "sess1.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected file at %s: %v", want, err)
	}
}

func TestArchive_AppendsAsJSONLLines(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	ts := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	for i, content := range []string{"first", "second", "third"} {
		if err := w.Append(archive.Event{
			Timestamp: ts.Add(time.Duration(i) * time.Second),
			AgentID:   "main", SessionID: "sess1", Role: "user", Content: content,
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "archive", "2026-05", "sess1.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), data)
	}
	for i, line := range lines {
		var e archive.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %d not valid JSON: %v", i, err)
		}
	}
}

func TestArchive_RotatesAcrossMonths(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	may := time.Date(2026, 5, 31, 23, 59, 0, 0, time.UTC)
	june := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, ts := range []time.Time{may, june} {
		if err := w.Append(archive.Event{
			Timestamp: ts, AgentID: "main", SessionID: "sess1", Role: "user", Content: "x",
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	for _, sub := range []string{"2026-05", "2026-06"} {
		if _, err := os.Stat(filepath.Join(dir, "archive", sub)); err != nil {
			t.Errorf("missing month dir %s: %v", sub, err)
		}
	}
}

func TestArchive_AppendRequiresSessionID(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	err := w.Append(archive.Event{AgentID: "main", Role: "user"})
	if err == nil {
		t.Fatal("Append with empty SessionID should error")
	}
}

func TestArchive_AppendFillsTimestampIfZero(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	before := time.Now().UTC()
	if err := w.Append(archive.Event{
		SessionID: "sess1", AgentID: "main", Role: "user", Content: "x",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	month := before.Format("2006-01")
	data, err := os.ReadFile(filepath.Join(dir, "archive", month, "sess1.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var e archive.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &e); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Timestamp.Before(before) {
		t.Fatalf("Timestamp %v before %v — not auto-filled", e.Timestamp, before)
	}
}

func TestArchive_SanitizesSessionIDPathTraversal(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	ts := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	// Malicious session ID with traversal characters.
	if err := w.Append(archive.Event{
		Timestamp: ts, SessionID: "../../etc/passwd", AgentID: "main", Role: "user", Content: "x",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// File MUST be under the archive root, not escaped.
	entries, _ := os.ReadDir(filepath.Join(dir, "archive", "2026-05"))
	if len(entries) != 1 {
		t.Fatalf("expected exactly one sanitized file, got %v", entries)
	}
	name := entries[0].Name()
	if strings.Contains(name, "..") || strings.Contains(name, "/") {
		t.Fatalf("sanitization failed; got filename %q", name)
	}
}

func TestArchive_StreamReadsBackInChronologicalOrder(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	for i, ts := range []time.Time{
		time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 16, 2, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC),
	} {
		content := []string{"a", "b", "c"}[i]
		session := "sess1"
		if i == 2 {
			session = "sess2"
		}
		if err := w.Append(archive.Event{
			Timestamp: ts, AgentID: "main", SessionID: session, Role: "user", Content: content,
		}); err != nil {
			t.Fatal(err)
		}
	}
	r := archive.NewReader(dir)
	var got []string
	for e, err := range r.Stream(archive.Filter{}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got = append(got, e.Content)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("Stream order = %v, want [a b c]", got)
	}
}

func TestArchive_StreamFiltersByAgentAndRole(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	ts := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	for i, ev := range []archive.Event{
		{Timestamp: ts.Add(0), AgentID: "alice", SessionID: "s1", Role: "user", Content: "alice-user"},
		{Timestamp: ts.Add(1), AgentID: "alice", SessionID: "s1", Role: "assistant", Content: "alice-asst"},
		{Timestamp: ts.Add(2), AgentID: "bob", SessionID: "s2", Role: "user", Content: "bob-user"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	r := archive.NewReader(dir)
	var got []string
	for e, err := range r.Stream(archive.Filter{AgentID: "alice", Role: "user"}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e.Content)
	}
	if len(got) != 1 || got[0] != "alice-user" {
		t.Fatalf("filter result = %v, want [alice-user]", got)
	}
}

func TestArchive_StreamMissingArchiveDirReturnsNothing(t *testing.T) {
	dir := t.TempDir() // archive subdir does not exist
	r := archive.NewReader(dir)
	count := 0
	for _, err := range r.Stream(archive.Filter{}) {
		if err != nil {
			t.Fatalf("stream over empty: %v", err)
		}
		count++
	}
	if count != 0 {
		t.Fatalf("got %d events from empty archive, want 0", count)
	}
}

func TestArchive_StreamSurfacesMalformedLineAsError(t *testing.T) {
	dir := t.TempDir()
	month := filepath.Join(dir, "archive", "2026-05")
	if err := os.MkdirAll(month, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a valid event then a malformed line.
	good := `{"ts":"2026-05-16T00:00:00Z","agent_id":"a","session_id":"s","role":"user","content":"good"}`
	if err := os.WriteFile(filepath.Join(month, "s.jsonl"), []byte(good+"\n{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := archive.NewReader(dir)
	var contents []string
	var sawError bool
	for e, err := range r.Stream(archive.Filter{}) {
		if err != nil {
			sawError = true
			continue
		}
		contents = append(contents, e.Content)
	}
	if !sawError {
		t.Fatal("expected malformed line to surface as error")
	}
	if len(contents) != 1 || contents[0] != "good" {
		t.Fatalf("good line not preserved: got %v", contents)
	}
}

func TestArchive_PathForLayout(t *testing.T) {
	got := archive.PathFor("/tmp/x", time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC), "sess_abc")
	if !strings.HasSuffix(got, filepath.Join("archive", "2026-05", "sess_abc.jsonl")) {
		t.Fatalf("PathFor = %q, want archive/2026-05/sess_abc.jsonl suffix", got)
	}
}

func TestArchive_AppendIsConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			err := w.Append(archive.Event{
				Timestamp: time.Date(2026, 5, 16, 0, 0, i, 0, time.UTC),
				AgentID:   "main", SessionID: "sess1", Role: "user",
				Content: strings.Repeat("x", 100),
			})
			done <- err
		}(i)
	}
	for i := 0; i < 10; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent Append: %v", err)
		}
	}
	// Verify all 10 lines made it.
	data, err := os.ReadFile(filepath.Join(dir, "archive", "2026-05", "sess1.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 10 {
		t.Fatalf("got %d lines, want 10 (concurrency may have lost writes)", len(lines))
	}
	// Each line must be valid JSON (no torn writes).
	for i, l := range lines {
		var e archive.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Errorf("line %d torn: %v", i, err)
		}
	}
}

func TestArchive_DefaultDirReturnsAbsolutePath(t *testing.T) {
	got, err := archive.DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if got == "" || !filepath.IsAbs(got) {
		t.Fatalf("DefaultDir = %q; want non-empty absolute path", got)
	}
	if !strings.Contains(got, "tenant") {
		t.Errorf("DefaultDir = %q; want to contain 'tenant'", got)
	}
}

func TestArchive_ToolCallAndResultRoundtrip(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	ts := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	if err := w.Append(archive.Event{
		Timestamp: ts, AgentID: "main", SessionID: "s", Role: "assistant",
		ToolCalls: []archive.ToolCall{{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(archive.Event{
		Timestamp: ts.Add(time.Second), AgentID: "main", SessionID: "s", Role: "tool",
		ToolResult: &archive.ToolResult{CallID: "call_1", Content: "result text"},
	}); err != nil {
		t.Fatal(err)
	}
	r := archive.NewReader(dir)
	var events []archive.Event
	for e, err := range r.Stream(archive.Filter{}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if len(events[0].ToolCalls) != 1 || events[0].ToolCalls[0].Name != "search" {
		t.Errorf("ToolCalls did not roundtrip: %+v", events[0].ToolCalls)
	}
	if events[1].ToolResult == nil || events[1].ToolResult.CallID != "call_1" {
		t.Errorf("ToolResult did not roundtrip: %+v", events[1].ToolResult)
	}
}

// Sanity: ensure ErrNotFound is the right kind of error for callers
// using errors.Is. (Archive itself doesn't have ErrNotFound, but the
// pattern is documented at the package level.)
var _ = errors.Is
