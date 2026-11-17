package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// spoolDir, under the logs root, holds events written while no Tenant
// process was up to take them (§6.4). One file is one writer's batch: one
// event per line, in the event's wire form (event.Event's JSON).
const spoolDir = "spool"

// Spool leaves evs in the spool under root (the logs root) for the
// housekeeper to file, as every writer must: into a .tmp file, flushed to
// the disk, closed, then renamed <name>.ndjson (fsperm.Replace), so a file
// is never read half-written or, after a power cut, empty.
func Spool(root string, evs ...event.Event) error {
	var b bytes.Buffer
	for _, e := range evs {
		j, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b.Write(append(j, '\n'))
	}
	dir := filepath.Join(root, spoolDir)
	if err := fsperm.MkdirAll(dir); err != nil {
		return err
	}
	name := filepath.Join(dir, strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+runtrace.NewID()[:8])
	return fsperm.Replace(name+".ndjson", b.Bytes())
}

// drainSpool files the spool's finished files: it claims each <name>.ndjson
// by renaming it <name>.ingesting, files its events and deletes it. A claim
// or a delete that fails (on Windows, while another process has the file
// open) is tried again at the next run, and a .tmp is never read. A
// .ingesting left by a run that stopped part way is filed again, skipping
// what the store already holds.
func (h *Hub) drainSpool(ctx context.Context, st *store.Store) error {
	dir := filepath.Join(h.root, spoolDir)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		switch filepath.Ext(path) {
		case ".ndjson":
			claimed := strings.TrimSuffix(path, ".ndjson") + ".ingesting"
			if os.Rename(path, claimed) != nil {
				continue
			}
			path = claimed
		case ".ingesting":
		default:
			continue
		}
		if err := h.fileSpool(ctx, st, path); err != nil {
			errs = append(errs, err)
			continue
		}
		_ = os.Remove(path) // else filed again next run, as held
	}
	return errors.Join(errs...)
}

// fileSpool files one claimed spool file. What it can't file is dropped,
// counted and reported.
func (h *Hub) fileSpool(ctx context.Context, st *store.Store, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	bad, why := 0, ""
	// The writer closed the file before renaming it: its last line is whole
	// with or without a newline.
	err = scanLines(io.MultiReader(f, strings.NewReader("\n")), func(lines []string, _ int64) error {
		evs := make([]event.Event, 0, len(lines))
		for _, ln := range lines {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			e, err := h.spoolEvent(ln, fi.ModTime())
			if err != nil {
				if bad++; why == "" {
					why = err.Error()
				}
				continue
			}
			evs = append(evs, e)
		}
		_, err := h.commitNew(ctx, st, evs)
		return err
	})
	if err == nil && bad > 0 {
		catalog.SpoolRefused.Log(ctx, "file", filepath.Base(path), "lines", bad, "err", why)
	}
	return err
}

// spoolEvent reads one spool line: an event in its wire form, filed as
// External keeps it, as POST /api/events does. It can't name a log or a
// provider only Tenant writes; with no time it takes its file's.
func (h *Hub) spoolEvent(line string, at time.Time) (event.Event, error) {
	var e event.Event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return e, err
	}
	if e.Provider == "" {
		e.Provider = e.Log
	}
	switch {
	case !ValidSource(e.Log) || !event.ValidName(e.Provider):
		return e, fmt.Errorf("log %q or provider %q is not a valid name", e.Log, e.Provider)
	case Reserved(e.Log) || Reserved(e.Provider):
		return e, fmt.Errorf("log %q or provider %q is Tenant's own", e.Log, e.Provider)
	}
	if e.Time.IsZero() {
		e.Time = at
	}
	e.Time = e.Time.UTC()
	External(&e)
	h.scrubEvent(&e, h.opts.MaxFieldBytes)
	return e, nil
}
