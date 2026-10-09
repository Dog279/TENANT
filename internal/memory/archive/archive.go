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
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/model"
)

// Event is one archived occurrence. Schema is intentionally stable —
// adding fields is fine, renaming or removing is a migration. The
// JSON tag names won't change even if Go field names refactor.
type Event struct {
	Timestamp  time.Time      `json:"ts"`
	AgentID    string         `json:"agent_id"`
	SessionID  string         `json:"session_id"`
	TraceID    string         `json:"trace_id,omitempty"` // the turn's run record (runs.db)
	Role       string         `json:"role"`               // user | assistant | tool | system
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
	CallID  string                `json:"call_id"`
	Content string                `json:"content,omitempty"`
	IsError bool                  `json:"is_error,omitempty"`
	Labels  []model.InfoFlowLabel `json:"labels,omitempty"`
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

// WriterOptions controls archive durability.
type WriterOptions struct {
	// SyncInterval is the number of successful writes to a given archive file
	// between fsyncs. Zero and one both select the default per-event sync, so a
	// successful Append acknowledges a durable event. Values greater than one
	// opt into batching for higher throughput and can lose up to
	// SyncInterval-1 acknowledged events per file on power loss; callers should
	// call Flush during graceful shutdown.
	SyncInterval int
}

// Writer is the append-only archive writer. Safe for concurrent Append calls
// and Flush calls. Each Append opens, appends, optionally syncs, and closes the
// target file. Per-session file caching is deliberately avoided so file
// descriptors are not retained indefinitely.
type Writer struct {
	baseDir      string
	syncInterval int
	mu           sync.Mutex
	pending      map[string]int
	checkedTail  map[string]bool
}

// NewWriter returns an archive Writer rooted at baseDir. baseDir is
// the Tenant data root (e.g. ~/.local/share/tenant); the writer
// attaches /archive under it.
func NewWriter(baseDir string) *Writer {
	return NewWriterWithOptions(baseDir, WriterOptions{})
}

// BaseDir is the data directory the archive lives under.
func (w *Writer) BaseDir() string { return w.baseDir }

// NewWriterWithOptions returns an archive Writer with an explicit durability
// policy. NewWriter should be preferred unless measured fsync cost justifies a
// batched SyncInterval and the caller can Flush during graceful shutdown.
func NewWriterWithOptions(baseDir string, opts WriterOptions) *Writer {
	interval := opts.SyncInterval
	if interval < 2 {
		interval = 1
	}
	return &Writer{
		baseDir:      baseDir,
		syncInterval: interval,
		pending:      make(map[string]int),
		checkedTail:  make(map[string]bool),
	}
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

	if err := fsperm.MkdirAll(filepath.Dir(path)); err != nil {
		return fmt.Errorf("archive: mkdir %s: %w", filepath.Dir(path), err)
	}
	if !w.checkedTail[path] {
		if err := repairTruncatedTail(path); err != nil {
			return err
		}
		w.checkedTail[path] = true
	}
	f, err := openArchiveFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		return fmt.Errorf("archive: open %s: %w", path, err)
	}
	record := append(line, '\n')
	pending := w.pending[path] + 1
	syncNow := pending >= w.syncInterval

	n, writeErr := f.Write(record)
	if writeErr == nil && n != len(record) {
		writeErr = io.ErrShortWrite
	}
	var syncErr error
	if writeErr == nil && syncNow {
		syncErr = f.Sync()
	}
	closeErr := f.Close()

	if writeErr == nil {
		if syncNow && syncErr == nil {
			delete(w.pending, path)
		} else {
			w.pending[path] = pending
		}
	} else {
		// A failed or short write may have left an unterminated final record.
		// Recheck and repair the tail before the next append from this writer.
		w.checkedTail[path] = false
	}

	var errs []error
	if writeErr != nil {
		errs = append(errs, fmt.Errorf("archive: write %s: %w", path, writeErr))
	}
	if syncErr != nil {
		errs = append(errs, fmt.Errorf("archive: sync %s: %w", path, syncErr))
	}
	if closeErr != nil {
		errs = append(errs, fmt.Errorf("archive: close %s: %w", path, closeErr))
	}
	return errors.Join(errs...)
}

// Flush syncs every archive file with pending writes from a batched writer.
// It is safe to call for the default per-event policy, where it is normally a
// no-op. A path remains pending when opening or syncing it fails, allowing a
// later Flush to retry.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	paths := make([]string, 0, len(w.pending))
	for path := range w.pending {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var errs []error
	for _, path := range paths {
		f, err := openArchiveFile(path, os.O_RDWR)
		if err != nil {
			errs = append(errs, fmt.Errorf("archive: open for flush %s: %w", path, err))
			continue
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr == nil {
			delete(w.pending, path)
		} else {
			errs = append(errs, fmt.Errorf("archive: sync %s: %w", path, syncErr))
		}
		if closeErr != nil {
			errs = append(errs, fmt.Errorf("archive: close %s: %w", path, closeErr))
		}
	}
	return errors.Join(errs...)
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
	sc.Split(splitCompleteJSONLines)
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

// splitCompleteJSONLines accepts only newline-terminated records. Append always
// writes a trailing newline, so an unterminated final token is deterministically
// treated as a torn write and skipped while all preceding records remain
// replayable. Complete malformed records still surface parse errors.
func splitCompleteJSONLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) != 0 {
		return len(data), nil, nil
	}
	return 0, nil, nil
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

// SessionFileName is the file a session's events are archived under, in every
// month directory the session spans.
func SessionFileName(sessionID string) string { return sanitizeSession(sessionID) + ".jsonl" }

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

// repairTruncatedTail removes an unterminated final JSONL record before a
// reopened writer appends. The truncation itself is synced so a subsequent
// append cannot turn the torn bytes into a permanent malformed record.
func repairTruncatedTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("archive: open for recovery %s: %w", path, err)
	}

	info, statErr := f.Stat()
	if statErr != nil {
		closeErr := f.Close()
		return errors.Join(
			fmt.Errorf("archive: stat for recovery %s: %w", path, statErr),
			wrapCloseError(path, closeErr),
		)
	}
	if info.Size() == 0 {
		return wrapCloseError(path, f.Close())
	}

	last := []byte{0}
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		closeErr := f.Close()
		return errors.Join(
			fmt.Errorf("archive: read for recovery %s: %w", path, err),
			wrapCloseError(path, closeErr),
		)
	}
	if last[0] == '\n' {
		return wrapCloseError(path, f.Close())
	}

	const blockSize = 64 * 1024
	buf := make([]byte, blockSize)
	truncateAt := int64(0)
	for end := info.Size(); end > 0; {
		start := end - int64(len(buf))
		if start < 0 {
			start = 0
		}
		n, readErr := f.ReadAt(buf[:end-start], start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			closeErr := f.Close()
			return errors.Join(
				fmt.Errorf("archive: read for recovery %s: %w", path, readErr),
				wrapCloseError(path, closeErr),
			)
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			truncateAt = start + int64(i) + 1
			break
		}
		end = start
	}

	truncateErr := f.Truncate(truncateAt)
	var syncErr error
	if truncateErr == nil {
		syncErr = f.Sync()
	}
	closeErr := f.Close()
	var errs []error
	if truncateErr != nil {
		errs = append(errs, fmt.Errorf("archive: truncate torn tail %s: %w", path, truncateErr))
	}
	if syncErr != nil {
		errs = append(errs, fmt.Errorf("archive: sync recovered file %s: %w", path, syncErr))
	}
	if err := wrapCloseError(path, closeErr); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func wrapCloseError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("archive: close %s: %w", path, err)
}

// --- Test support ---

type archiveFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// testFileFactory overrides the private-file open for tests. Nil restores the
// default.
var testFileFactory func(path string, flag int) (archiveFile, error)

// openArchiveFile opens through fsperm so archives stay owner-only (TEN-328).
func openArchiveFile(path string, flag int) (archiveFile, error) {
	if testFileFactory != nil {
		return testFileFactory(path, flag)
	}
	f, err := fsperm.OpenFile(path, flag)
	if err != nil {
		return nil, err
	}
	return f, nil
}
