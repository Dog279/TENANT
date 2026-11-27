package backup

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/logs/event"
	"tenant/internal/secrets"
	"tenant/internal/version"
)

// Options says what Create backs up and how.
type Options struct {
	// ConfigDir and DataDir are the directories to back up. They may be the
	// same directory (the macOS default); they must not otherwise nest.
	ConfigDir string
	DataDir   string
	// Secrets are the absolute paths of Tenant's at-rest secret files. An
	// encrypted backup carries them; an unencrypted one leaves them out and
	// lists them as skipped.
	Secrets []string
	// ConfigFile reports whether a path, relative to a directory that is
	// both the config and the data dir, is config. Those files are filed
	// under the config root so a restore onto separate directories puts
	// them where Tenant looks. Unused when the directories differ.
	ConfigFile func(rel string) bool
	// NotIncluded is recorded in the manifest as skipped: things Tenant uses
	// that live outside both directories, such as a wiki vault.
	NotIncluded []Skipped
	// LogsDir is the log book's root (logs.dir); empty means <data>/logs.
	// IncludeMirror also captures its text mirror, which logbook.db
	// already holds (logbook.go).
	LogsDir       string
	IncludeMirror bool
	Keys          Keys
	// Now stamps the backup; tests fix it.
	Now func() time.Time
}

// Result describes a written backup.
type Result struct {
	Path     string
	Manifest *Manifest
	// Bytes is the size of the backup file.
	Bytes int64
	// Warnings are problems that did not stop the backup, such as a
	// destination filesystem that cannot restrict access to the file.
	Warnings []string
}

// stagingPrefix names Create's working directory inside the data dir. The
// walk skips anything with this prefix, including one a crash left behind.
const stagingPrefix = ".backup-staging-"

// Create writes a backup of the config and data dirs to out. Every SQLite
// store is captured with VACUUM INTO and every JSONL log up to its last
// complete record, so a running Tenant can keep working while a backup is
// taken. Stores that refer to others are captured before the ones they
// refer to, and the archive, which everything else points back to, is
// captured last: a reference in the backup always finds its target. The
// file is written under a temporary name and renamed into place, owner-only.
func Create(ctx context.Context, out string, opts Options) (_ *Result, err error) {
	ctx, act := event.Start(ctx, CreateRun, "path", out)
	defer act.End(&err)
	cfg, data, err := cleanRoots(opts.ConfigDir, opts.DataDir)
	if err != nil {
		return nil, err
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(absOut); err == nil {
		return nil, fmt.Errorf("backup: %s already exists", absOut)
	}
	for _, root := range []string{cfg, data} {
		if within(absOut, root) {
			return nil, fmt.Errorf("backup: write the backup outside Tenant's directories (%s is inside %s)", absOut, root)
		}
	}
	recipients, err := opts.Keys.recipients()
	if err != nil {
		return nil, err
	}
	encrypted := len(recipients) > 0
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	m := &Manifest{
		Format:        FormatVersion,
		TenantVersion: version.String(),
		CreatedAt:     now().UTC().Truncate(time.Second),
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		Encrypted:     encrypted,
		Secrets:       encrypted,
	}
	plan, skipped, err := planSources(cfg, data, opts, encrypted)
	if err != nil {
		return nil, err
	}
	m.Skipped = append(skipped, opts.NotIncluded...)

	staging, err := os.MkdirTemp(data, stagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("backup: staging dir: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := fsperm.RestrictDir(staging); err != nil {
		return nil, err
	}
	for i, src := range plan {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dst := filepath.Join(staging, fmt.Sprintf("%06d", i))
		entry, err := capture(ctx, src, dst)
		if errors.Is(err, fs.ErrNotExist) {
			// Deleted between the walk and the copy, like a finished
			// temp file; the backup is of what exists.
			m.Skipped = append(m.Skipped, Skipped{Path: src.entryPath, Reason: "removed while the backup ran"})
			continue
		}
		if err != nil {
			return nil, err
		}
		m.Entries = append(m.Entries, entry)
		plan[i].staged = dst
	}
	staged := make(map[string]string, len(plan))
	for _, src := range plan {
		if src.staged != "" {
			staged[src.entryPath] = src.staged
		}
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("backup: refusing to write an invalid manifest: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(absOut), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(absOut), "."+filepath.Base(absOut)+".*.partial")
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	var warnings []string
	// Owner-only access matters for an unencrypted backup. A destination
	// that cannot express it (a FAT or exFAT drive) is worth a warning, not
	// a failed backup.
	if err := secrets.ProtectFile(tmpPath); err != nil && !encrypted {
		warnings = append(warnings, fmt.Sprintf("could not restrict access to the backup file (%v); it holds your data unencrypted, so keep it somewhere private or use --encrypt", err))
	}
	open := func(e Entry) (*os.File, error) { return os.Open(staged[e.Path]) }
	if err := writeStream(tmp, m, open, recipients); err != nil {
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := publish(tmpPath, absOut); err != nil {
		return nil, err
	}
	keep = true
	info, err := os.Stat(absOut)
	if err != nil {
		return nil, err
	}
	Written.Log(ctx, "path", absOut, "bytes", info.Size(), "files", len(m.Entries), "databases", m.Count(KindSQLite),
		"logs", m.Count(KindLog), "secrets", m.Count(KindSecret), "skipped", len(m.Skipped), "encrypted", encrypted, "version", m.TenantVersion)
	for _, w := range warnings {
		CreateWarning.Log(ctx, "path", absOut, "warning", w)
	}
	return &Result{Path: absOut, Manifest: m, Bytes: info.Size(), Warnings: warnings}, nil
}

// publish gives the finished temporary file its final name without replacing
// anything already there: a hard link fails when the name exists. On a
// filesystem without hard links it checks, then renames.
func publish(tmp, final string) error {
	err := os.Link(tmp, final)
	if err == nil {
		_ = os.Remove(tmp) // the backup is in place; a stray temp name is harmless
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("backup: %s already exists", final)
	}
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("backup: %s already exists", final)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// source is one file the backup will capture.
type source struct {
	abs       string
	entryPath string // the manifest path, "config/…" or "data/…"
	kind      Kind
	rank      int
	staged    string
}

// capture copies one source into dst and describes it.
func capture(ctx context.Context, src source, dst string) (Entry, error) {
	switch src.kind {
	case KindSQLite:
		if _, err := os.Stat(src.abs); err != nil {
			return Entry{}, err
		}
		if err := snapshotSQLite(ctx, src.abs, dst); err != nil {
			return Entry{}, fmt.Errorf("backup: %s: %w", src.entryPath, err)
		}
		if err := fsperm.RestrictFile(dst); err != nil {
			return Entry{}, err
		}
	case KindLog:
		if err := copyLog(src.abs, dst); err != nil {
			return Entry{}, wrapCapture(src, err)
		}
	case KindSecret:
		data, err := secrets.Peek(src.abs)
		if err != nil {
			return Entry{}, wrapCapture(src, err)
		}
		if err := fsperm.WriteFile(dst, data); err != nil {
			return Entry{}, err
		}
	default:
		if err := copyFile(src.abs, dst); err != nil {
			return Entry{}, wrapCapture(src, err)
		}
	}
	size, sum, err := hashFile(dst)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Path: src.entryPath, Kind: src.kind, Size: size, SHA256: sum}, nil
}

func wrapCapture(src source, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return fmt.Errorf("backup: %s: %w", src.entryPath, err)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := fsperm.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// captureOrder ranks stores so that a store is captured before the stores it
// refers to: facts cite episodes and runs, skills cite runs, the distill
// cursor names an episode, episodes cite runs and route decisions, and every
// store traces back to the archive. Anything unlisted sits in the middle.
var captureOrder = map[string]int{
	"data/facts.db":       0,
	"data/skills.db":      1,
	"data/tenant_meta.db": 2,
	"data/usage.db":       3,
	"data/episodes.db":    5,
	"data/runs.db":        6,
}

func rankOf(entryPath string, kind Kind) int {
	if r, ok := captureOrder[entryPath]; ok {
		return r
	}
	switch {
	case kind == KindSQLite:
		return 4
	case kind == KindLog && strings.HasPrefix(entryPath, "data/archive/"):
		return 30
	case kind == KindLog:
		return 20
	default:
		return 10
	}
}

// wikiIndex matches the wiki plugin's search-index sidecars in <data>/wiki.
var wikiIndex = regexp.MustCompile(`^[0-9a-f]{1,16}\.json$`)

// builtinSkip leaves out what a restore must not bring back or can rebuild.
func builtinSkip(root, rel string, name string) (string, bool) {
	switch {
	case root == RootData && rel == datalock.FileName:
		return "", true
	case root == RootData && (path.Dir(rel) == "logs/boots" || path.Dir(rel) == "logs/crash"):
		// Boot records and crash files of the processes running at backup
		// time: restored, they would read as processes that died (TEN-365).
		return "", true
	case strings.HasPrefix(name, stagingPrefix):
		return "", true
	case strings.HasSuffix(name, ".tmp"), strings.HasSuffix(name, ".partial"),
		strings.HasSuffix(name, ".part"), name == ".doctor-write-probe":
		return "temporary file", true
	case strings.Contains(name, ".corrupted-"):
		return "damaged database kept by doctor --fix", true
	case strings.Contains(name, ".corrupt-"):
		return "damaged log book moved aside at open", true
	case name == "logbook.lock": // held by every process with the log book open
		return "", true
	case root == RootData && path.Dir(rel) == "wiki" && wikiIndex.MatchString(name):
		return "wiki search index; rebuilt on next use", true
	}
	return "", false
}

// sqliteSidecar reports a -wal, -shm or -journal file next to its database.
// VACUUM INTO already reads through them.
func sqliteSidecar(abs string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if base, ok := strings.CutSuffix(abs, suffix); ok {
			if info, err := os.Stat(base); err == nil && info.Mode().IsRegular() {
				return true
			}
		}
	}
	return false
}

// planSources walks the roots and decides, file by file, what to capture and
// how, in capture order.
func planSources(cfg, data string, opts Options, encrypted bool) ([]source, []Skipped, error) {
	secretSet := make(map[string]bool, len(opts.Secrets))
	for _, s := range opts.Secrets {
		if abs, err := filepath.Abs(s); err == nil {
			secretSet[canonical(abs)] = true
		}
	}
	shared := samePath(cfg, data)
	type walkRoot struct{ name, dir string }
	roots := []walkRoot{{RootData, data}}
	if !shared {
		roots = append([]walkRoot{{RootConfig, cfg}}, roots...)
	}
	logsDir := cmp.Or(opts.LogsDir, filepath.Join(data, "logs"))
	mirrorLeft := "" // the mirror's files, as a pattern, once one is left out
	var plan []source
	var skipped []Skipped
	for _, r := range roots {
		err := filepath.WalkDir(r.dir, func(abs string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && abs == r.dir {
					return filepath.SkipDir
				}
				return err
			}
			if abs == r.dir {
				return nil
			}
			relOS, err := filepath.Rel(r.dir, abs)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(relOS)
			root := r.name
			if shared && opts.ConfigFile != nil && opts.ConfigFile(rel) {
				root = RootConfig
			}
			entryPath := root + "/" + rel
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), stagingPrefix) {
					return filepath.SkipDir
				}
				return nil
			}
			// An empty lock file holds nothing, and one a running Tenant holds
			// cannot be read on Windows: the fleet's, under serve, failed the
			// backup every update starts with (TEN-556). Whoever needs one
			// makes it again. A lock file with something in it (a package
			// manager's, in a work folder) is a file like any other.
			if strings.HasSuffix(d.Name(), ".lock") {
				if info, err := d.Info(); err == nil && info.Size() == 0 {
					return nil
				}
			}
			if reason, skip := builtinSkip(root, rel, d.Name()); skip {
				if reason != "" {
					skipped = append(skipped, Skipped{Path: entryPath, Reason: reason})
				}
				return nil
			}
			if !d.Type().IsRegular() {
				skipped = append(skipped, Skipped{Path: entryPath, Reason: "not a regular file (symlink or special file)"})
				return nil
			}
			if sqliteSidecar(abs) {
				return nil
			}
			if err := validEntryPath(entryPath); err != nil {
				skipped = append(skipped, Skipped{Path: entryPath, Reason: "name cannot be restored portably"})
				return nil
			}
			kind := KindFile
			db, mirror := logbookFile(abs, logsDir)
			switch {
			case mirror && !opts.IncludeMirror:
				mirrorLeft = path.Dir(path.Dir(entryPath)) + "/*/*.log"
				return nil
			case db:
				kind = KindSQLite // never opened raw: see logbook.go
			case secretSet[canonical(abs)]:
				if !encrypted {
					skipped = append(skipped, Skipped{Path: entryPath, Reason: "secret; only encrypted backups carry secrets"})
					return nil
				}
				kind = KindSecret
			case strings.HasSuffix(d.Name(), ".jsonl"):
				kind = KindLog
			default:
				ok, err := isSQLite(abs)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				if ok {
					kind = KindSQLite
				}
			}
			plan = append(plan, source{abs: abs, entryPath: entryPath, kind: kind, rank: rankOf(entryPath, kind)})
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("backup: walk %s: %w", r.dir, err)
		}
	}
	if mirrorLeft != "" {
		skipped = append(skipped, Skipped{Path: mirrorLeft, Reason: "the log book's text mirror; logbook.db holds the same events (--include-mirror takes it too)"})
	}
	sort.SliceStable(plan, func(i, j int) bool {
		if plan[i].rank != plan[j].rank {
			return plan[i].rank < plan[j].rank
		}
		return plan[i].entryPath < plan[j].entryPath
	})
	return plan, skipped, nil
}

// cleanRoots makes both directories absolute and refuses a layout a restore
// could not put back: one directory nested inside the other.
func cleanRoots(cfg, data string) (string, string, error) {
	if cfg == "" || data == "" {
		return "", "", errors.New("backup: both the config and data dirs are required")
	}
	c, err := filepath.Abs(cfg)
	if err != nil {
		return "", "", err
	}
	d, err := filepath.Abs(data)
	if err != nil {
		return "", "", err
	}
	if !samePath(c, d) && (within(c, d) || within(d, c)) {
		return "", "", fmt.Errorf("backup: the config dir (%s) and data dir (%s) are nested; use one directory for both or two separate ones", c, d)
	}
	return c, d, nil
}

// canonical folds case where the filesystem usually does.
func canonical(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool { return canonical(a) == canonical(b) }

// within reports whether p is root or inside it.
func within(p, root string) bool {
	rel, err := filepath.Rel(canonical(root), canonical(p))
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
