package logs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/runtrace"
)

const (
	defaultRingCap      = 10000
	defaultRetention    = 30 * 24 * time.Hour
	defaultMaxField     = 4096
	defaultMaxFileBytes = 64 << 20
	backfillLines       = 300       // lines re-read per source from its newest file at start / discovery
	backfillMaxBytes    = 512 << 10 // and at most this much of that file
	tailReadMax         = 1 << 20   // per tick, per foreign file
	scanEvery           = 5 * time.Second
	tailEvery           = time.Second
	pruneEvery          = time.Hour
	sourcesScanMinGap   = 2 * time.Second
)

// Options tunes a Hub. Zero values pick the defaults noted per field.
type Options struct {
	RingCap       int           // retained entries for the feed (0 ⇒ 10000)
	Retention     time.Duration // files older than this are deleted (0 ⇒ 30 days; <0 ⇒ never)
	MaxFieldBytes int           // per-value cap in files + ring (0 ⇒ 4096; <0 ⇒ unlimited)
	MaxFileBytes  int64         // a day's file rolls to <day>.N.log past this (0 ⇒ 64 MiB)
	MinLevel      slog.Level    // records below this are dropped (zero ⇒ INFO; use slog.LevelDebug for everything)
	Now           func() time.Time
}

// SourceInfo describes one source (= one folder = one dashboard tab).
type SourceInfo struct {
	Name   string
	Native bool      // written by this process; false = a folder discovered on disk and tailed
	Count  int       // entries currently retained in the ring
	LastAt time.Time // newest retained entry
	Err    string    // last file write / tail error ("" when healthy)
}

// FileInfo describes one file inside a source folder.
type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Hub is the single log sink. Safe for concurrent use. Open one per process
// (Install it for package-level For loggers), Start it in daemons so folders
// dropped by other processes get discovered and tailed, Close it on exit.
type Hub struct {
	root  string // "" ⇒ memory-only (ring, no files, no discovery)
	opts  Options
	epoch string // names this hub's Seq numbering, which starts at 1 on every Open

	// scrub runs on every entry before the value cap, the ring and the files.
	scrub Scrubber

	mu       sync.Mutex
	ring     []Entry
	next     uint64
	sources  map[string]*source
	lastScan time.Time
	closed   bool
	subs     map[chan struct{}]struct{} // one wake-up channel per Subscribe
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	pipe atomic.Pointer[pipeline] // set once a store is attached (pipeline.go)
}

type source struct {
	name   string
	dir    string
	native bool
	count  int
	lastAt time.Time
	err    string

	// native writer
	f     *os.File
	fday  string
	fidx  int
	fsize int64

	// foreign tail (a folder some other process writes)
	tailPath string
	tailOff  int64
	partial  []byte
}

// Open creates (or reopens) the hub rooted at root. root == "" gives a
// memory-only hub (tests, or a process with no data dir): the ring works, files
// and discovery are off. The root directory is created if missing.
func Open(root string, opts Options) (*Hub, error) {
	if opts.RingCap <= 0 {
		opts.RingCap = defaultRingCap
	}
	if opts.Retention == 0 {
		opts.Retention = defaultRetention
	}
	if opts.MaxFieldBytes == 0 {
		opts.MaxFieldBytes = defaultMaxField
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = defaultMaxFileBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if root != "" {
		root = filepath.Clean(root)
		if err := fsperm.MkdirAll(root); err != nil {
			return nil, fmt.Errorf("logs: create %s: %w", root, err)
		}
	}
	return &Hub{
		root:    root,
		opts:    opts,
		epoch:   runtrace.BootID(), // one hub per process
		next:    1,
		sources: map[string]*source{},
		subs:    map[chan struct{}]struct{}{},
		stop:    make(chan struct{}),
	}, nil
}

// NewMemory is Open("", …) for tests and file-less callers: a ring of the given
// capacity, no disk.
func NewMemory(capacity int) *Hub {
	h, _ := Open("", Options{RingCap: capacity})
	return h
}

// Root is the on-disk log directory ("" for a memory-only hub).
func (h *Hub) Root() string { return h.root }

// Start backfills the ring from the newest file of every existing source
// folder (so the feed shows history from before this process started), then
// runs the background loops — folder discovery, foreign-file tailing and
// retention pruning — until ctx is done or Close is called. No-op for a
// memory-only hub.
func (h *Hub) Start(ctx context.Context) {
	if h.root == "" {
		return
	}
	h.mu.Lock()
	added := h.scanLocked()
	var seed []Entry
	for _, s := range added {
		seed = append(seed, h.tailInitLocked(s)...)
	}
	sort.SliceStable(seed, func(i, j int) bool { return seed[i].At.Before(seed[j].At) })
	for _, e := range seed {
		h.appendLocked(e)
	}
	h.pruneLocked()
	h.mu.Unlock()
	if len(seed) > 0 {
		h.wake()
	}
	h.wg.Add(1)
	go h.run(ctx)
}

func (h *Hub) run(ctx context.Context) {
	defer h.wg.Done()
	scanT := time.NewTicker(scanEvery)
	tailT := time.NewTicker(tailEvery)
	pruneT := time.NewTicker(pruneEvery)
	defer scanT.Stop()
	defer tailT.Stop()
	defer pruneT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stop:
			return
		case <-tailT.C:
			h.tailTick()
		case <-scanT.C:
			h.scanTick()
		case <-pruneT.C:
			h.mu.Lock()
			h.pruneLocked()
			h.mu.Unlock()
		}
	}
}

// Close stops the loops and closes every open file. Idempotent.
func (h *Hub) Close() error {
	if p := h.pipe.Swap(nil); p != nil {
		p.close() // flushes into the files and the store while they are open
	}
	h.stopOnce.Do(func() { close(h.stop) })
	h.wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, s := range h.sources {
		if s.f != nil {
			_ = s.f.Close()
			s.f = nil
		}
	}
	return nil
}

func (h *Hub) now() time.Time { return h.opts.Now() }

// Log records one entry: it is written to <root>/<source>/<day>.log, appended
// to the ring with the next sequence number, and open feeds are woken. An
// unknown source is created on the spot (folder + tab). Never blocks on a
// reader and never panics: a file that cannot be written is noted on the
// source (SourceInfo.Err) and the entry still reaches the ring.
func (h *Hub) Log(e Entry) {
	if e.Level < h.opts.MinLevel {
		return
	}
	if p := h.pipe.Load(); p != nil && p.logEntry(e) {
		return // the pipeline scrubs, caps, mirrors and commits it
	}
	if !ValidSource(e.Source) {
		if n := NormalizeSource(e.Source); n != "" {
			e.Source = n
		} else {
			e.Source = DefaultSource
		}
	}
	if e.At.IsZero() {
		e.At = h.now()
	}
	e = h.scrub.Entry(e, h.opts.MaxFieldBytes)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	s := h.sourceLocked(e.Source, true)
	if h.root != "" {
		h.writeLocked(s, e.At, e.Line())
	}
	h.appendLocked(e)
	h.mu.Unlock()
	h.wake()
}

// Logf is Log for a plain message: Logf("discord", slog.LevelInfo, "relay up", "guild", id).
// kv are alternating key, value pairs (values are fmt.Sprint-ed).
func (h *Hub) Logf(source string, level slog.Level, msg string, kv ...any) {
	e := Entry{Source: source, Level: level, Msg: msg}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Fields = append(e.Fields, Field{Key: fmt.Sprint(kv[i]), Value: fmt.Sprint(kv[i+1])})
	}
	h.Log(e)
}

// sourceLocked returns the source record for name, creating it on first use.
// native=true marks it as written by this process; a folder previously tailed
// as foreign is drained to EOF first so nothing it already holds is lost, then
// tailing stops (we are the writer now, and would otherwise re-read our own
// lines).
func (h *Hub) sourceLocked(name string, native bool) *source {
	s := h.sources[name]
	if s == nil {
		s = &source{name: name}
		if h.root != "" {
			s.dir = filepath.Join(h.root, name)
		}
		h.sources[name] = s
	}
	if native && !s.native {
		if s.tailPath != "" {
			h.tailReadLocked(s)
		}
		s.native = true
	}
	return s
}

func (h *Hub) appendLocked(e Entry) {
	e.Seq = h.next
	h.next++
	h.ring = append(h.ring, e)
	s := h.sourceLocked(e.Source, false)
	s.count++
	if e.At.After(s.lastAt) {
		s.lastAt = e.At
	}
	if over := len(h.ring) - h.opts.RingCap; over > 0 {
		for _, old := range h.ring[:over] {
			if os := h.sources[old.Source]; os != nil && os.count > 0 {
				os.count--
			}
		}
		h.ring = h.ring[over:] // Seq stays monotonic; a cursor never goes stale
	}
}

func (h *Hub) wake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe returns a wake-up channel of the caller's own and a cancel func
// that drops it. Every write wakes every subscriber; wake-ups coalesce, so a
// receive means "new entries exist — call Since". Subscribe before the first
// Since for a gap-free live tail.
func (h *Hub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Epoch names this hub's sequence numbering. Seq starts again at 1 each time
// a hub opens, so a cursor is only meaningful with the epoch it came from: a
// client holding another epoch's cursor re-syncs from the start of the ring.
func (h *Hub) Epoch() string { return h.epoch }

// Since returns every retained entry with Seq > cursor (oldest first) plus the
// new head cursor to pass next time. A cursor older than the ring returns the
// whole ring (the caller backfills what survived). Lossless within retention.
func (h *Hub) Since(cursor uint64) ([]Entry, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	head := h.next - 1
	if len(h.ring) == 0 || cursor >= head {
		return nil, head
	}
	// Binary search: the ring is Seq-sorted.
	i := sort.Search(len(h.ring), func(i int) bool { return h.ring[i].Seq > cursor })
	out := make([]Entry, len(h.ring)-i)
	copy(out, h.ring[i:])
	return out, head
}

// Snapshot returns the full retained backlog (oldest first) + the head cursor.
func (h *Hub) Snapshot() ([]Entry, uint64) { return h.Since(0) }

// Head is the current head cursor.
func (h *Hub) Head() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.next - 1
}

// Sources lists every known source — natively written this run, seeded from
// disk at start, or discovered since — in display order: the built-in
// subsystems first (turns, sub-agents, approvals, the channels…), then any
// custom source alphabetically. A folder created by another process shows up
// here within a couple of seconds.
func (h *Hub) Sources() []SourceInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.root != "" && !h.closed && h.now().Sub(h.lastScan) > sourcesScanMinGap {
		for _, s := range h.scanLocked() {
			for _, e := range h.tailInitLocked(s) {
				h.appendLocked(e)
			}
		}
	}
	out := make([]SourceInfo, 0, len(h.sources))
	for _, s := range h.sources {
		out = append(out, SourceInfo{Name: s.name, Native: s.native, Count: s.count, LastAt: s.lastAt, Err: s.err})
	}
	sort.Slice(out, func(i, j int) bool { return SourceLess(out[i].Name, out[j].Name) })
	return out
}

// preferredOrder pins the built-in subsystems to a stable, meaningful order in
// the dashboard's tab row; everything else follows alphabetically. Its keys
// are Tenant's own sources (Builtin).
var preferredOrder = map[string]int{
	"turns": 0, "subagents": 1, "approvals": 2, "discord": 3, "imessage": 4, "cron": 5,
	"eval": 6, "improve": 7, "model": 8, "route": 9, "memory": 10, "mcp": 11,
	"peering": 12, "dashboard": 13, "sandbox": 14, "privacy": 15, "shutdown": 16,
	"system": 99,
}

// Builtin reports whether name is one of Tenant's own sources. The logging
// API refuses to write into them, so an outside script can't forge an
// approval or a turn.
func Builtin(name string) bool {
	_, ok := preferredOrder[name]
	return ok
}

// SourceLess is the tab/display ordering: built-ins in preferredOrder, then
// custom sources alphabetically, "system" last.
func SourceLess(a, b string) bool {
	ra, oka := preferredOrder[a]
	rb, okb := preferredOrder[b]
	switch {
	case oka && okb:
		return ra < rb
	case oka:
		return ra < preferredOrder["system"]
	case okb:
		return rb == preferredOrder["system"]
	}
	return a < b
}

// --- files ------------------------------------------------------------------

func (h *Hub) writeLocked(s *source, at time.Time, line string) {
	day := at.Format("2006-01-02")
	if s.f == nil || s.fday != day {
		h.rollLocked(s, day)
	}
	if s.f == nil {
		return
	}
	if s.fsize+int64(len(line))+1 > h.opts.MaxFileBytes {
		_ = s.f.Close()
		s.f = nil
		s.fidx++
		h.openFileLocked(s, day)
		if s.f == nil {
			return
		}
	}
	n, err := s.f.WriteString(line + "\n")
	if err != nil {
		s.err = "write: " + err.Error()
		return
	}
	s.fsize += int64(n)
	s.err = ""
}

// rollLocked switches the writer to the given day's file, resuming on the
// newest size-rolled segment that already exists for that day (so a restart
// keeps appending where the last run stopped rather than reopening segment 0).
func (h *Hub) rollLocked(s *source, day string) {
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	s.fidx = 0
	if ents, err := os.ReadDir(s.dir); err == nil {
		for _, e := range ents {
			if d, idx, ok := splitSegment(e.Name()); ok && d == day && idx > s.fidx {
				s.fidx = idx
			}
		}
	}
	h.openFileLocked(s, day)
	if s.f != nil && s.fsize >= h.opts.MaxFileBytes {
		_ = s.f.Close()
		s.f = nil
		s.fidx++
		h.openFileLocked(s, day)
	}
}

// segmentRe matches our own file names: <day>.log or <day>.<n>.log.
var segmentRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:\.(\d+))?\.log$`)

// splitSegment parses a daily file name into its day and segment index.
func splitSegment(name string) (day string, idx int, ok bool) {
	m := segmentRe.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0, false
		}
		idx = n
	}
	return m[1], idx, true
}

// fileLess orders a folder listing newest first: daily files by day then
// segment index (descending), then any other names descending.
func fileLess(a, b string) bool {
	da, ia, oka := splitSegment(a)
	db, ib, okb := splitSegment(b)
	switch {
	case oka && okb:
		if da != db {
			return da > db
		}
		return ia > ib
	case oka:
		return true
	case okb:
		return false
	}
	return a > b
}

func (h *Hub) openFileLocked(s *source, day string) {
	name := day + ".log"
	if s.fidx > 0 {
		name = fmt.Sprintf("%s.%d.log", day, s.fidx)
	}
	if err := fsperm.MkdirAll(s.dir); err != nil {
		s.err = "mkdir: " + err.Error()
		return
	}
	f, err := fsperm.OpenFile(filepath.Join(s.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		s.err = "open: " + err.Error()
		return
	}
	var size int64
	if st, serr := f.Stat(); serr == nil {
		size = st.Size()
	}
	s.f, s.fday, s.fsize = f, day, size
}

// fileRe is the shape of a readable file name inside a source folder: no
// separators, no leading dot, a log/txt suffix.
var fileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.(log|txt)$`)

// Files lists the files in a source folder, newest name first (daily files
// sort chronologically by name).
func (h *Hub) Files(source string) ([]FileInfo, error) {
	if h.root == "" {
		return nil, nil
	}
	if !ValidSource(source) {
		return nil, fmt.Errorf("logs: invalid source %q", source)
	}
	ents, err := os.ReadDir(filepath.Join(h.root, source))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []FileInfo
	for _, e := range ents {
		if e.IsDir() || !fileRe.MatchString(e.Name()) {
			continue
		}
		fi, ferr := statOpen(filepath.Join(h.root, source, e.Name()))
		if ferr != nil {
			continue
		}
		out = append(out, FileInfo{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return fileLess(out[i].Name, out[j].Name) })
	return out, nil
}

// statOpen stats a file through a handle. On Windows the directory listing
// reports a stale size for a file another handle still has open, which is
// exactly today's file while the hub appends to it.
func statOpen(path string) (os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// OpenFile opens one file of a source for reading (raw view / download). Both
// names are validated to their strict shapes, so a request can never reach
// outside the source folder.
func (h *Hub) OpenFile(source, name string) (io.ReadCloser, FileInfo, error) {
	if h.root == "" {
		return nil, FileInfo{}, os.ErrNotExist
	}
	if !ValidSource(source) || !fileRe.MatchString(name) {
		return nil, FileInfo{}, fmt.Errorf("logs: invalid file %q/%q", source, name)
	}
	f, err := os.Open(filepath.Join(h.root, source, name))
	if err != nil {
		return nil, FileInfo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, FileInfo{}, err
	}
	return f, FileInfo{Name: name, Size: st.Size(), ModTime: st.ModTime()}, nil
}

// --- discovery + tailing ------------------------------------------------------

// recordDirs hold records, not logs: serve's and the TUI's boot records, and
// crash dumps waiting to be filed (TEN-365). Tailed as sources, a dump would
// flood the feed one stack line at a time.
var recordDirs = map[string]bool{"boots": true, "crash": true}

// scanLocked registers every valid folder under root not yet known as a
// FOREIGN source (some other process writes it) and returns the new ones.
func (h *Hub) scanLocked() []*source {
	h.lastScan = h.now()
	ents, err := os.ReadDir(h.root)
	if err != nil {
		return nil
	}
	var added []*source
	for _, e := range ents {
		if !e.IsDir() || !ValidSource(e.Name()) || recordDirs[e.Name()] {
			continue
		}
		if _, known := h.sources[e.Name()]; known {
			continue
		}
		s := h.sourceLocked(e.Name(), false)
		added = append(added, s)
	}
	return added
}

func (h *Hub) scanTick() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	var seed []Entry
	for _, s := range h.scanLocked() {
		seed = append(seed, h.tailInitLocked(s)...)
	}
	// A foreign source's newest file may have changed (a new day, a rotation):
	// finish the old one, then follow the new one from its start.
	for _, s := range h.sources {
		if s.native {
			continue
		}
		newest := newestFile(s.dir)
		if newest != "" && newest != s.tailPath {
			if s.tailPath != "" {
				h.tailReadLocked(s)
			}
			s.tailPath, s.tailOff, s.partial = newest, 0, nil
			h.tailReadLocked(s)
		}
	}
	for _, e := range seed {
		h.appendLocked(e)
	}
	h.mu.Unlock()
	if len(seed) > 0 {
		h.wake()
	}
}

func (h *Hub) tailTick() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	var got int
	for _, s := range h.sources {
		if s.native || s.tailPath == "" {
			continue
		}
		got += h.tailReadLocked(s)
	}
	h.mu.Unlock()
	if got > 0 {
		h.wake()
	}
}

// tailInitLocked seeds a source from the tail of its newest file (the last
// backfillLines lines) and positions the foreign tail at EOF. Returns the
// seed entries (not yet appended — the caller orders them).
func (h *Hub) tailInitLocked(s *source) []Entry {
	newest := newestFile(s.dir)
	if newest == "" {
		return nil
	}
	lines, size := tailLines(newest, backfillLines, backfillMaxBytes)
	s.tailPath, s.tailOff, s.partial = newest, size, nil
	out := make([]Entry, 0, len(lines))
	for _, ln := range lines {
		if e, ok := h.lineEntry(s, ln); ok {
			out = append(out, e)
		}
	}
	return out
}

// tailReadLocked ingests whatever a foreign file gained since the last read.
// Returns the number of entries appended.
func (h *Hub) tailReadLocked(s *source) int {
	if s.tailPath == "" {
		return 0
	}
	st, err := os.Stat(s.tailPath)
	if err != nil {
		s.err = "tail: " + err.Error()
		return 0
	}
	if st.Size() < s.tailOff { // truncated / rewritten: start over
		s.tailOff, s.partial = 0, nil
	}
	if st.Size() == s.tailOff {
		return 0
	}
	f, err := os.Open(s.tailPath)
	if err != nil {
		s.err = "tail: " + err.Error()
		return 0
	}
	defer f.Close()
	if _, err := f.Seek(s.tailOff, io.SeekStart); err != nil {
		s.err = "tail: " + err.Error()
		return 0
	}
	buf, err := io.ReadAll(io.LimitReader(f, tailReadMax))
	if err != nil {
		s.err = "tail: " + err.Error()
		return 0
	}
	s.tailOff += int64(len(buf))
	s.err = ""
	data := append(s.partial, buf...)
	n := 0
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break
		}
		line := string(data[:nl])
		data = data[nl+1:]
		if e, ok := h.lineEntry(s, line); ok {
			h.appendLocked(e)
			n++
		}
	}
	s.partial = append([]byte(nil), data...)
	return n
}

// lineEntry turns one line read from disk into a ring entry. A line in our
// format parses fully; anything else (a script's free-form output) is kept
// verbatim as an INFO message stamped now — never dropped. The folder, not the
// line, decides the source.
func (h *Hub) lineEntry(s *source, line string) (Entry, bool) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return Entry{}, false
	}
	e, ok := ParseLine(line)
	if !ok {
		e = Entry{At: h.now(), Level: slog.LevelInfo, Msg: line}
	}
	e.Source = s.name
	if e.Level < h.opts.MinLevel {
		return Entry{}, false
	}
	// Older files and other writers may hold secrets. ponytail: the line is
	// scrubbed whole under h.mu (at most tailReadMax a tick); window it as Log
	// does if a flooding foreign writer ever shows up.
	return h.scrub.Entry(e, 0), true
}

// newestFile picks the file to follow in a folder: the most recently modified
// readable log file ("" when none).
func newestFile(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestT time.Time
	for _, e := range ents {
		if e.IsDir() || !fileRe.MatchString(e.Name()) {
			continue
		}
		fi, ferr := e.Info()
		if ferr != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestT) || (fi.ModTime().Equal(bestT) && e.Name() > filepath.Base(best)) {
			best, bestT = filepath.Join(dir, e.Name()), fi.ModTime()
		}
	}
	return best
}

// tailLines returns the last n complete lines of path (reading at most
// maxBytes from its end) and the file's size.
func tailLines(path string, n int, maxBytes int64) ([]string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0
	}
	size := st.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, size
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, size
	}
	text := string(buf)
	if start > 0 { // drop the partial first line
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			text = text[nl+1:]
		} else {
			text = ""
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, size
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, size
}

// --- retention ----------------------------------------------------------------

// SetRetention changes a running hub's retention (as Options.Retention: 0 ⇒
// 30 days, <0 ⇒ never) and prunes with it at once, so a new setting needs no
// restart.
func (h *Hub) SetRetention(d time.Duration) {
	if d == 0 {
		d = defaultRetention
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opts.Retention = d
	h.pruneLocked()
}

var dayPrefixRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})`)

// pruneLocked deletes files older than the retention window in every source
// folder (by the date in the file name when it has one, else by mtime). The
// file currently being written or tailed is never removed.
func (h *Hub) pruneLocked() {
	if h.root == "" || h.opts.Retention < 0 {
		return
	}
	now := h.now()
	for _, s := range h.sources {
		if s.dir == "" {
			continue
		}
		ents, err := os.ReadDir(s.dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !fileRe.MatchString(e.Name()) {
				continue
			}
			p := filepath.Join(s.dir, e.Name())
			if (s.f != nil && filepath.Base(s.f.Name()) == e.Name()) || p == s.tailPath {
				continue
			}
			var stamp time.Time
			if m := dayPrefixRe.FindStringSubmatch(e.Name()); m != nil {
				if d, derr := time.ParseInLocation("2006-01-02", m[1], time.Local); derr == nil {
					stamp = d.Add(24 * time.Hour) // the day's file is "written" until the day ends
				}
			}
			if stamp.IsZero() {
				fi, ferr := e.Info()
				if ferr != nil {
					continue
				}
				stamp = fi.ModTime()
			}
			if now.Sub(stamp) > h.opts.Retention {
				_ = os.Remove(p)
			}
		}
	}
}
