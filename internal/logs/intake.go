package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The intake (docs/design/logging-v2.md §6.4): the housekeeper files into
// the store what no Tenant process wrote through its pipeline. Each run it
// imports v1's history (once), drains the spool (spool.go) and reads the
// foreign folders. What it files is tagged imported or external and goes
// straight to the store: it is never mirrored, and never fed to the v1 ring,
// which tails the folders itself.

// The keywords the intake tags events with.
const (
	KeywordImported = "imported" // v1's history
	KeywordExternal = "external" // a foreign folder's line or a spooled event: written outside Tenant
)

const (
	intakeBatch = 512     // lines per commit
	maxLine     = 1 << 20 // a longer line is cut to this
	historyKey  = "intake.v1"
	foreignKey  = "intake.ext." // + the folder: its files' offsets
)

// intakeState is what the intake keeps between the runs of the process
// holding the lease.
type intakeState struct {
	sizes map[string]int64 // each foreign file's size at the last run
}

// intake is the housekeeping task. The history is planned first, once the
// store holds its first event, so nothing the intake files can move the
// line between v1's history and what the store has.
func (h *Hub) intake(ctx context.Context) error {
	st := h.store()
	if st == nil || h.root == "" {
		return nil
	}
	own, err := h.ownLogs(ctx, st)
	if err != nil {
		return err
	}
	planned, err := h.importHistory(ctx, st, own)
	if !planned {
		return err
	}
	return errors.Join(err, h.drainSpool(ctx, st), h.readForeign(ctx, st, own))
}

// tenantNames are the names only code inside the binary writes: the
// Security log, the built-in sources, and every registered provider's name
// and log. Anything under tenant. is reserved too (reserved).
func tenantNames() map[string]bool {
	names := map[string]bool{store.SecurityLog: true}
	for n := range preferredOrder {
		names[n] = true
	}
	for _, p := range event.Providers() {
		names[p.Name], names[p.Log] = true, true
	}
	return names
}

func reserved(names map[string]bool, name string) bool {
	return names[name] || strings.HasPrefix(name, "tenant.")
}

// sealed logs are never filed from text: the Security log and tenant.*.
func sealed(name string) bool {
	return name == store.SecurityLog || strings.HasPrefix(name, "tenant.")
}

// ownLogs are the logs whose folders are Tenant's mirror: tenantNames, every
// log the store holds, and every log this process writes. The rest of the
// folders are foreign.
func (h *Hub) ownLogs(ctx context.Context, st *store.Store) (map[string]bool, error) {
	own := tenantNames()
	held, err := st.Logs(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range held {
		own[l.Name] = true
	}
	h.mu.Lock()
	for n, s := range h.sources {
		if s.native {
			own[n] = true
		}
	}
	h.mu.Unlock()
	return own, nil
}

// folders lists the log folders under the root: records and the spool are
// not logs.
func (h *Hub) folders() []string {
	ents, _ := os.ReadDir(h.root)
	var out []string
	for _, e := range ents {
		if e.IsDir() && ValidSource(e.Name()) && !recordDirs[e.Name()] {
			out = append(out, e.Name())
		}
	}
	return out
}

// logFiles lists the readable files of a folder, by name.
func logFiles(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if !e.IsDir() && fileRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// --- v1's history ---------------------------------------------------------------

// history is the v1 import's plan and progress, kept in meta.
type history struct {
	Cutoff time.Time        `json:"cutoff"`          // the store's first event: lines before it are v1's
	Files  map[string]int64 `json:"files,omitempty"` // <folder>/<file> left to read → bytes read
	Total  int              `json:"total"`           // files planned
	Lines  int              `json:"lines"`           // lines filed so far
	Done   bool             `json:"done,omitempty"`
}

// importHistory imports v1's text files once (§6.4): the lines of every
// Tenant log's folder written before the store's first event, scrubbed, as
// events of that log tagged imported. Its plan and progress are in meta, so
// a run cut short resumes, and skips what it already filed. planned is false
// until the store holds an event to plan from.
func (h *Hub) importHistory(ctx context.Context, st *store.Store, own map[string]bool) (planned bool, err error) {
	var hs history
	raw, err := st.Meta(ctx, historyKey)
	switch {
	case err != nil:
		return false, err
	case raw != "":
		if err := json.Unmarshal([]byte(raw), &hs); err != nil {
			return false, fmt.Errorf("logs: %s in meta: %w", historyKey, err)
		}
		if hs.Done {
			return true, nil
		}
	default:
		first, _, _, err := st.Since(ctx, store.Cursor{StoreID: st.StoreID()}, 1)
		if err != nil || len(first) == 0 {
			return false, err
		}
		hs = history{Cutoff: first[0].Time, Files: map[string]int64{}}
		last := hs.Cutoff.Local().Format(time.DateOnly)
		for _, dir := range h.folders() {
			if !own[dir] || sealed(dir) {
				continue // a foreign folder is read whole by readForeign
			}
			for _, name := range logFiles(filepath.Join(h.root, dir)) {
				if day, _, ok := splitSegment(name); !ok || day <= last {
					hs.Files[dir+"/"+name] = 0
				}
			}
		}
		hs.Total = len(hs.Files)
		if err := saveMeta(ctx, st, historyKey, hs); err != nil {
			return false, err
		}
	}
	for _, key := range slices.Sorted(maps.Keys(hs.Files)) {
		if err := h.importFile(ctx, st, &hs, key); err != nil {
			return true, err
		}
		delete(hs.Files, key)
		if err := saveMeta(ctx, st, historyKey, hs); err != nil {
			return true, err
		}
	}
	hs.Done = true
	if err := saveMeta(ctx, st, historyKey, hs); err != nil {
		return true, err
	}
	catalog.HistoryImported.Log(ctx, "lines", hs.Lines, "files", hs.Total)
	return true, nil
}

// importFile imports one file of the plan from where the last run left it.
func (h *Hub) importFile(ctx context.Context, st *store.Store, hs *history, key string) error {
	dir, name, _ := strings.Cut(key, "/")
	f, err := os.Open(filepath.Join(h.root, dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // retention got there first
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	off := hs.Files[key]
	at := fi.ModTime() // a line not in the v1 format takes the time of the line before it
	return scanLines(io.NewSectionReader(f, off, fi.Size()-off), func(lines []string, used int64) error {
		var evs []event.Event
		for _, ln := range lines {
			e, ok := h.lineEvent(ln, dir, KeywordImported, at, 0, false)
			if !ok {
				continue
			}
			if at = e.Time; e.Time.Before(hs.Cutoff) {
				evs = append(evs, e)
			}
		}
		n, err := h.commitNew(ctx, st, evs)
		if err != nil {
			return err
		}
		hs.Lines += n
		hs.Files[key] = off + used
		return saveMeta(ctx, st, historyKey, hs)
	})
}

// --- foreign folders --------------------------------------------------------------

// readForeign files the lines of every foreign folder, one no Tenant log
// owns (§6.4), as events of ext.<folder> tagged external. Each file's offset
// is in meta, so a restart files no line twice. Only what a file held at the
// last run is read: a Tenant process mirrors a line before it commits it, so
// the folder of a log's first line looks foreign for that moment, and must
// not be filed as another process's external log.
func (h *Hub) readForeign(ctx context.Context, st *store.Store, own map[string]bool) error {
	sizes := map[string]int64{}
	defer func() { h.in.sizes = sizes }()
	var errs []error
	for _, dir := range h.folders() {
		if log := NormalizeSource("ext." + dir); !own[dir] && !sealed(dir) && log != "" {
			errs = append(errs, h.readFolder(ctx, st, dir, log, sizes))
		}
	}
	return errors.Join(errs...)
}

func (h *Hub) readFolder(ctx context.Context, st *store.Store, dir, log string, sizes map[string]int64) error {
	key := foreignKey + dir
	offs := map[string]int64{}
	if raw, err := st.Meta(ctx, key); err != nil {
		return err
	} else if raw != "" {
		if err := json.Unmarshal([]byte(raw), &offs); err != nil {
			return fmt.Errorf("logs: %s in meta: %w", key, err)
		}
	}
	files := logFiles(filepath.Join(h.root, dir))
	gone := len(offs)
	maps.DeleteFunc(offs, func(name string, _ int64) bool { return !slices.Contains(files, name) })
	for _, name := range files {
		path := filepath.Join(h.root, dir, name)
		fi, err := statOpen(path)
		if err != nil {
			continue
		}
		sizes[path] = fi.Size()
		limit, seen := h.in.sizes[path]
		off := offs[name]
		if off > fi.Size() {
			off = 0 // cut short or rewritten: from the top, as the tail does
		}
		if limit = min(limit, fi.Size()); !seen || limit <= off {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		err = scanLines(io.NewSectionReader(f, off, limit-off), func(lines []string, used int64) error {
			now := h.now()
			var evs []event.Event
			for _, ln := range lines {
				if e, ok := h.lineEvent(ln, log, KeywordExternal, now, h.opts.MaxFieldBytes, false); ok {
					evs = append(evs, e)
				}
			}
			// ponytail: the offset is saved after the commit, so a crash
			// between them files a batch's free-form lines again (lines in
			// the v1 format are skipped); save it in the commit's transaction
			// if that ever matters.
			if _, err := h.commitNew(ctx, st, evs); err != nil {
				return err
			}
			offs[name] = off + used
			return saveMeta(ctx, st, key, offs)
		})
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	if len(offs) < gone {
		return saveMeta(ctx, st, key, offs)
	}
	return nil
}

// --- after an outage ------------------------------------------------------------

// importMirror files the mirror lines written in [from, to) for every Tenant
// log into the store, tagged keyword, skipping the lines the store already
// holds, and returns how many it filed. During an outage the pipeline wrote
// its batches to the mirror only; this brings them back. A line keeps its
// event ID. The Security log is never read back: its records are committed,
// or refused, as they are written.
func (h *Hub) importMirror(ctx context.Context, from, to time.Time, keyword string) (int, error) {
	st := h.store()
	if st == nil || h.root == "" {
		return 0, nil
	}
	own, err := h.ownLogs(ctx, st)
	if err != nil {
		return 0, err
	}
	from = from.Truncate(time.Millisecond) // a line keeps milliseconds
	first, last := from.Local().Format(time.DateOnly), to.Local().Format(time.DateOnly)
	total := 0
	for _, dir := range h.folders() {
		if !own[dir] || sealed(dir) {
			continue
		}
		for _, name := range logFiles(filepath.Join(h.root, dir)) {
			if day, _, ok := splitSegment(name); !ok || day < first || day > last {
				continue
			}
			n, err := h.recoverFile(ctx, st, filepath.Join(h.root, dir, name), dir, from, to, keyword)
			if total += n; err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

func (h *Hub) recoverFile(ctx context.Context, st *store.Store, path, log string, from, to time.Time, keyword string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	total := 0
	err = scanLines(f, func(lines []string, _ int64) error {
		var evs []event.Event
		for _, ln := range lines {
			if e, ok := h.lineEvent(ln, log, keyword, time.Time{}, 0, true); ok && !e.Time.Before(from) && e.Time.Before(to) {
				evs = append(evs, e)
			}
		}
		n, err := h.commitNew(ctx, st, evs)
		total += n
		return err
	})
	return total, err
}

// --- lines to events --------------------------------------------------------------

// scanLines reads the complete lines of r, a last line with no newline
// being left for a later read, and calls fn with each batch of up to
// intakeBatch lines and the bytes of r read up to the batch's end. A line
// past maxLine is cut to it.
func scanLines(r io.Reader, fn func(lines []string, used int64) error) error {
	br := bufio.NewReaderSize(r, maxLine)
	var lines []string
	var used int64
	for {
		b, err := br.ReadSlice('\n')
		line, n := string(b), int64(len(b))
		for err == bufio.ErrBufferFull {
			b, err = br.ReadSlice('\n')
			n += int64(len(b))
		}
		if err == nil {
			used += n
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
		if len(lines) == intakeBatch || err != nil && len(lines) > 0 {
			if ferr := fn(lines, used); ferr != nil {
				return ferr
			}
			lines = nil
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// lineEvent reads one line of a text file as an event of log, tagged kw. A
// line in the v1 format keeps its time, level and fields; any other line is
// its message, at info, stamped at. Its text is scrubbed and cut at max
// (<= 0: not cut; Tenant cut its own lines as it wrote them). An event= field
// is an outside line's claim to be a catalogued event and is dropped, unless
// recovered: then the line is Tenant's own mirror, which must parse, and
// keeps its event ID unless it names a sealed provider.
func (h *Hub) lineEvent(line, log, kw string, at time.Time, max int, recovered bool) (event.Event, bool) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return event.Event{}, false
	}
	en, ok := ParseLine(line)
	if !ok {
		if recovered {
			return event.Event{}, false
		}
		en = Entry{At: at, Level: slog.LevelInfo, Msg: line}
	}
	en.Source = log
	en.Fields = slices.DeleteFunc(en.Fields, func(f Field) bool {
		if f.Key != "event" {
			return false
		}
		p, _, ok := parseEventID(f.Value)
		return !recovered || !ok || sealed(p)
	})
	e := EventFromEntry(en)
	h.scrubEvent(&e, max)
	e.Keywords = append(slices.Clip(e.Keywords), kw)
	return e, true
}

// scrubEvent scrubs e's message, attributes and text data, and cuts each at
// max (<= 0: no cut), as the pipeline's flush does for what Tenant writes.
func (h *Hub) scrubEvent(e *event.Event, max int) {
	sc := &h.scrub
	e.Msg = sc.ScrubCap(e.Msg, max)
	for _, a := range attrs(e) {
		*a.val = sc.Field(a.key, *a.val, max)
	}
	for j, f := range e.Data {
		switch v := f.Value.(type) {
		case string:
			e.Data[j].Value = sc.Field(f.Key, v, max)
		case json.RawMessage:
			if s := sc.Field(f.Key, string(v), max); s != string(v) {
				e.Data[j].Value = json.RawMessage(s)
			}
		}
	}
}

// commitNew commits the events of evs the store doesn't hold yet, as their
// logs' policies admit them, and returns how many it committed. An event is
// held when one of its log has its millisecond and, flattened as the mirror
// writes it, its message: what a text line keeps of an event.
func (h *Hub) commitNew(ctx context.Context, st *store.Store, evs []event.Event) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	type stamp struct {
		ms  int64
		msg string
	}
	span := map[string][2]time.Time{}
	for _, e := range evs {
		s, ok := span[e.Log]
		if !ok || e.Time.Before(s[0]) {
			s[0] = e.Time
		}
		if !ok || e.Time.After(s[1]) {
			s[1] = e.Time
		}
		span[e.Log] = s
	}
	held := map[string]map[stamp]int{}
	for log, s := range span {
		m := map[stamp]int{}
		err := st.Stamps(ctx, log, s[0].Truncate(time.Millisecond), s[1].Truncate(time.Millisecond).Add(time.Millisecond),
			func(at time.Time, msg string) { m[stamp{at.UnixMilli(), flattenMsg(msg)}]++ })
		if err != nil {
			return 0, err
		}
		held[log] = m
	}
	p := h.pipe.Load()
	keep := make([]event.Event, 0, len(evs))
	for i := range evs {
		e := &evs[i]
		if k := (stamp{e.Time.UnixMilli(), flattenMsg(e.Msg)}); held[e.Log][k] > 0 {
			held[e.Log][k]--
			continue
		}
		if p == nil || p.admit(e) {
			keep = append(keep, *e)
		}
	}
	if err := st.Commit(ctx, keep); err != nil {
		return 0, err
	}
	return len(keep), nil
}

func saveMeta(ctx context.Context, st *store.Store, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return st.SetMeta(ctx, key, string(b))
}
