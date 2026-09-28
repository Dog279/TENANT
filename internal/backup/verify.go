package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/logs/event"
)

// Checks adds Tenant's knowledge of its own stores to Verify and Restore.
type Checks struct {
	// ReferenceSchema returns the tables and columns this build of Tenant
	// creates for the store at a manifest path, and false when the path is
	// not a store it knows.
	ReferenceSchema func(ctx context.Context, entryPath string) (Schema, bool, error)
	// CheckFile inspects a file Tenant knows, such as config.json. It
	// returns a non-empty newer when the file was written by a newer Tenant,
	// a note for anything else worth saying, and an error when the file is
	// damaged in a way that should block a restore.
	CheckFile func(entryPath, path string) (newer, note string, err error)
}

// Report is what verifying a backup found.
type Report struct {
	Manifest  *Manifest
	Encrypted bool
	// Problems are damage: a checksum that does not match, a truncated
	// stream, a database that fails integrity_check, a log line that is not
	// JSON. Any problem fails verify and blocks restore.
	Problems []string
	// Newer lists schema this build of Tenant does not know, from a backup
	// made by a newer Tenant. Restore refuses it unless forced.
	Newer []string
	// Notes are worth knowing but block nothing.
	Notes []string
}

// OK reports whether the backup verified without problems.
func (r *Report) OK() bool { return len(r.Problems) == 0 }

// Verify reads the whole backup at path into a private scratch directory
// under workDir (the system temp dir when empty) and checks it: every entry's
// size and checksum, the end of the stream, every SQLite store's integrity
// and schema, and every JSONL log's records. The scratch copy is removed
// before Verify returns. A damaged backup is reported in Report.Problems; the
// error is for failures to verify at all, like a missing file or a wrong
// passphrase.
func Verify(ctx context.Context, path string, keys Keys, checks Checks, workDir string) (_ *Report, err error) {
	ctx, act := event.Start(ctx, VerifyRun, "path", path)
	defer act.End(&err)
	scratch, err := os.MkdirTemp(workDir, "tenant-verify-")
	if err != nil {
		return nil, fmt.Errorf("backup: scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)
	roots := map[string]string{
		RootConfig: filepath.Join(scratch, RootConfig),
		RootData:   filepath.Join(scratch, RootData),
	}
	rep, err := readAndCheck(ctx, path, keys, checks, roots)
	if err == nil {
		logReport(ctx, path, rep)
	}
	return rep, err
}

// readAndCheck extracts the backup at path into roots and checks the result.
func readAndCheck(ctx context.Context, path string, keys Keys, checks Checks, roots map[string]string) (*Report, error) {
	ids, err := keys.identities()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := NewReader(f, ids)
	if err != nil {
		if errors.Is(err, ErrEncrypted) || errors.Is(err, ErrDecrypt) {
			return nil, err
		}
		// Anything else wrong with the manifest or header is damage.
		return &Report{Problems: []string{err.Error()}}, nil
	}
	rep := &Report{Manifest: r.Manifest(), Encrypted: r.Encrypted()}
	for _, dir := range roots {
		if err := emptyDir(dir); err != nil {
			return nil, err
		}
	}
	if err := extract(r, roots); err != nil {
		rep.Problems = append(rep.Problems, err.Error())
		return rep, nil
	}
	checkTree(ctx, rep, roots, checks)
	return rep, nil
}

// checkTree checks every extracted entry and records what it finds.
func checkTree(ctx context.Context, rep *Report, roots map[string]string, checks Checks) {
	for _, e := range rep.Manifest.Entries {
		root, rel, _ := strings.Cut(e.Path, "/")
		p := filepath.Join(roots[root], filepath.FromSlash(rel))
		switch e.Kind {
		case KindSQLite:
			if err := integrityCheck(ctx, p); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", e.Path, err))
				continue
			}
			if checks.ReferenceSchema == nil {
				continue
			}
			ref, known, err := checks.ReferenceSchema(ctx, e.Path)
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: reference schema: %v", e.Path, err))
				continue
			}
			if !known {
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s: not a store this Tenant knows; restored as is", e.Path))
				continue
			}
			got, err := ReadSchema(ctx, p)
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: read schema: %v", e.Path, err))
				continue
			}
			for _, extra := range got.Newer(ref) {
				// An unknown column can carry a constraint this Tenant's
				// inserts would violate; an unknown table is just ignored.
				if strings.HasPrefix(extra, "column ") {
					rep.Newer = append(rep.Newer, fmt.Sprintf("%s: %s", e.Path, extra))
				} else {
					rep.Notes = append(rep.Notes, fmt.Sprintf("%s: %s is not one this Tenant uses; left in place", e.Path, extra))
				}
			}
		case KindLog:
			if err := checkLog(p); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", e.Path, err))
			}
		case KindFile, KindSecret:
			if checks.CheckFile == nil {
				continue
			}
			newer, note, err := checks.CheckFile(e.Path, p)
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", e.Path, err))
			}
			if newer != "" {
				rep.Newer = append(rep.Newer, fmt.Sprintf("%s: %s", e.Path, newer))
			}
			if note != "" {
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s: %s", e.Path, note))
			}
		}
	}
}
