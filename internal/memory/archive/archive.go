// Package archive implements T5 of Tenant's memory architecture: the
// raw append-only event log. Every prompt, response, tool call, and
// tool result lands here as one JSONL line. Archive is the source of
// truth — every other tier (Episodic, Semantic) can be rebuilt from it.
//
// Layout:
//
//	{baseDir}/archive/2026-05/sess_abc123.jsonl
//	{baseDir}/archive/2026-05/sess_def456.jsonl
//	{baseDir}/archive/2026-06/sess_ghi789.jsonl
//
// Per-session files keep replay scoped, monthly directories keep ls(1)
// reasonable, append-only never deletes anything. If a user requests
// deletion (GDPR / personal cleanup), that's a separate tombstone
// flag in higher tiers — the archive stays write-once.
package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is one archived occurrence. Schema is intentionally stable —
// adding fields is fine, renaming or removing is a migration. The
// JSON tag names won't change even if Go field names refactor.
type Event struct {
	Timestamp  time.Time      `json:"ts"`
	AgentID    string         `json:"agent_id"`
	SessionID  string         `json:"session_id"`
	Role       string         `json:"role"` // user | assistant | tool | system
	Content    string         `json:"content,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolResult *ToolResult    `json:"tool_result,omitempty"`
	Metadata   map[string]any `json:"meta,omitempty"`
}

// ToolCall is a stable archive-side representation. Decoupled from
// model.ToolCall so internal LLM-interface changes don't ripple into
// the archive schema.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is the response from a tool back to the agent.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// DefaultDir returns the OS-appropriate base data directory.
// Honors $XDG_DATA_HOME on Linux. Use ArchiveDir(baseDir) to attach
// the /archive segment.
func DefaultDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "tenant"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "AppData", "Local", "tenant"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "tenant"), nil
	default: // linux + other unix
		if x := os.Getenv("XDG_DATA_HOME"); x != "" {
			return filepath.Join(x, "tenant"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "tenant"), nil
	}
}

// ArchiveDir returns the archive directory under baseDir.
func ArchiveDir(baseDir string) string {
	return filepath.Join(baseDir, "archive")
}

// PathFor returns the file path that an event with the given timestamp
// + session ID would write to.
func PathFor(baseDir string, ts time.Time, sessionID string) string {
	month := ts.UTC().Format("2006-01")
	safe := sanitizeSession(sessionID)
	return filepath.Join(ArchiveDir(baseDir), month, safe+".jsonl")
}

// Writer is the append-only archive writer. Safe for concurrent
// Append calls from multiple sessions; each Append opens, appends,
// and closes the target file. Per-session file caching is a v1.1
// optimization that wasn't worth the lock complexity for v1.
type Writer struct {
	baseDir string
	mu      sync.Mutex
}

// NewWriter returns an archive Writer rooted at baseDir. baseDir is
// the Tenant data root (e.g. ~/.local/share/tenant); the writer
// attaches /archive under it.
func NewWriter(baseDir string) *Writer {
	return &Writer{baseDir: baseDir}
}

// Reader returns a Reader over the same base directory, so a component holding
// the Writer can read back the raw transcript it has been appending — used by
// archive-sourced compaction (TEN-101) to summarize from ground truth.
func (w *Writer) Reader() *Reader { return NewReader(w.baseDir) }

// Append writes one event as a JSONL line to the file derived from
// (event.Timestamp, event.SessionID). Creates the monthly directory
// on demand. If event.Timestamp is zero, fills with time.Now().UTC().
func (w *Writer) Append(e Event) error {
	if e.SessionID == "" {
		return errors.New("archive: Event.SessionID required")
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	// A tool call with empty/blank Arguments is a valid call (no args), but an
	// empty json.RawMessage marshals to invalid JSON ("unexpected end of JSON
	// input") and would fail the whole append. Normalize to an empty object.
	for i := range e.ToolCalls {
		if len(bytes.TrimSpace(e.ToolCalls[i].Arguments)) == 0 {
			e.ToolCalls[i].Arguments = json.RawMessage("{}")
		}
	}
	line, err := json.Marshal(&e)
	if err != nil {
		return fmt.Errorf("archive: marshal event: %w", err)
	}
	path := PathFor(w.baseDir, e.Timestamp, e.SessionID)

	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("archive: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("archive: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("archive: write %s: %w", path, err)
	}
	return nil
}

// Filter narrows a Reader.Stream iteration. Zero-value Filter matches
// everything. Empty string fields are ignored; time fields default to
// open-ended (before zero = no upper bound, etc.).
type Filter struct {
	AgentID   string    // exact match if non-empty
	SessionID string    // exact match if non-empty
	Role      string    // exact match if non-empty
	After     time.Time // events with ts > After (zero = no lower bound)
	Before    time.Time // events with ts < Before (zero = no upper bound)
}

// Reader scans archive files. Reads are scoped to baseDir.
type Reader struct {
	baseDir string
}

// NewReader returns a reader rooted at baseDir.
func NewReader(baseDir string) *Reader {
	return &Reader{baseDir: baseDir}
}

// Stream yields events matching filter across all archive files in
// chronological order (by file path, then line). Uses Go 1.23+
// iterator. Caller breaks out of the range to stop.
func (r *Reader) Stream(filter Filter) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		files, err := r.listFiles(filter)
		if err != nil {
			yield(Event{}, err)
			return
		}
		for _, path := range files {
			if !streamFile(path, filter, yield) {
				return
			}
		}
	}
}

// listFiles returns archive file paths in chronological order, scoped
// by filter's time bounds.
func (r *Reader) listFiles(filter Filter) ([]string, error) {
	root := ArchiveDir(r.baseDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("archive: read root %s: %w", root, err)
	}
	var out []string
	for _, m := range entries {
		if !m.IsDir() {
			continue
		}
		// Filter monthly directories outside the time window.
		// Format is "2006-01"; we compare lexically because monthly
		// directories sort chronologically as strings.
		if !filter.After.IsZero() && m.Name() < filter.After.UTC().Format("2006-01") {
			continue
		}
		if !filter.Before.IsZero() && m.Name() > filter.Before.UTC().Format("2006-01") {
			continue
		}
		monthDir := filepath.Join(root, m.Name())
		files, err := os.ReadDir(monthDir)
		if err != nil {
			return nil, fmt.Errorf("archive: read %s: %w", monthDir, err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			if filter.SessionID != "" && strings.TrimSuffix(f.Name(), ".jsonl") != sanitizeSession(filter.SessionID) {
				continue
			}
			out = append(out, filepath.Join(monthDir, f.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func streamFile(path string, filter Filter, yield func(Event, error) bool) bool {
	f, err := os.Open(path)
	if err != nil {
		return yield(Event{}, fmt.Errorf("archive: open %s: %w", path, err))
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// Malformed line — surface and continue. Killing the
			// iteration over one bad row would hide everything after.
			if !yield(Event{}, fmt.Errorf("archive: parse %s: %w", path, err)) {
				return false
			}
			continue
		}
		if !filter.matches(e) {
			continue
		}
		if !yield(e, nil) {
			return false
		}
	}
	if err := sc.Err(); err != nil {
		return yield(Event{}, fmt.Errorf("archive: scan %s: %w", path, err))
	}
	return true
}

func (f Filter) matches(e Event) bool {
	if f.AgentID != "" && e.AgentID != f.AgentID {
		return false
	}
	if f.Role != "" && e.Role != f.Role {
		return false
	}
	if !f.After.IsZero() && !e.Timestamp.After(f.After) {
		return false
	}
	if !f.Before.IsZero() && !e.Timestamp.Before(f.Before) {
		return false
	}
	return true
}

// sanitizeSession strips path-traversal characters from a session ID.
// We don't get to trust caller-supplied IDs even though they're
// internal — a stray "../" turns the archive write into a path
// traversal bug.
func sanitizeSession(id string) string {
	if id == "" {
		return "_unknown"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
