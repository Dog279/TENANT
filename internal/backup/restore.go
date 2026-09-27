package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/secrets"
)

// RestoreOptions says where and how Restore puts a backup back.
type RestoreOptions struct {
	// ConfigDir and DataDir are the live directories to replace. They may be
	// the same directory; they must not otherwise nest.
	ConfigDir string
	DataDir   string
	Keys      Keys
	Checks    Checks
	// Secrets are the absolute paths of the live secret files. When the
	// backup carries no secrets (it was not encrypted), the live ones are
	// copied into the restored directories so the restore does not sign
	// Tenant out of everything.
	Secrets []string
	// Force restores a backup made by a newer Tenant.
	Force bool
	// DryRun verifies the backup in place of the live directories and
	// reports, changing nothing.
	DryRun bool
	// Now stamps the names of the kept directories; tests fix it.
	Now func() time.Time
}

// RestoreResult says what Restore did.
type RestoreResult struct {
	Report *Report
	// Aside are the replaced directories, kept under new names.
	Aside []string
	// KeptSecrets are live secret files carried over into the restored
	// directories, by manifest path.
	KeptSecrets []string
	// Restored is false for a dry run or a refused restore.
	Restored bool
}

// ErrRunning is returned when a Tenant process is using the data dir.
var ErrRunning = errors.New("backup: Tenant is running on this data dir (serve, the TUI or another command); stop it and try again")

// ErrNewer is returned when a backup comes from a newer Tenant and Force is
// not set.
var ErrNewer = errors.New("backup: the backup was made by a newer Tenant; upgrade Tenant, or restore anyway with --force")

// Restore replaces the config and data dirs with the backup at path. The
// backup is written to fresh directories beside the live ones and checked in
// full there (checksums, database integrity, schema, logs) before anything
// live is touched. Only then are the live directories renamed aside, kept
// as <dir>.pre-restore-<time>, and the restored ones renamed into place; if
// a rename fails, whatever already moved is put back. Restore refuses while
// any Tenant process holds the data dir.
func Restore(ctx context.Context, path string, opts RestoreOptions) (*RestoreResult, error) {
	cfg, data, err := cleanRoots(opts.ConfigDir, opts.DataDir)
	if err != nil {
		return nil, err
	}
	shared := samePath(cfg, data)
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	stamp := now().Format("20060102-150405")

	var lock *datalock.Lock
	if _, err := os.Stat(data); err == nil {
		lock, err = datalock.Exclusive(data)
		if errors.Is(err, datalock.ErrBusy) {
			return nil, ErrRunning
		}
		if err != nil {
			return nil, err
		}
	}
	defer lock.Release()

	stageData := data + ".restore-" + stamp
	stageCfg := cfg + ".restore-" + stamp
	if shared {
		stageCfg = stageData
	}
	cleanup := []string{stageData}
	if !shared {
		cleanup = append(cleanup, stageCfg)
	}
	restored := false
	defer func() {
		if !restored {
			for _, dir := range cleanup {
				_ = os.RemoveAll(dir)
			}
		}
	}()
	for _, dir := range cleanup {
		if _, err := os.Lstat(dir); err == nil {
			return nil, fmt.Errorf("backup: %s already exists; remove it or wait a second and retry", dir)
		}
	}
	roots := map[string]string{RootConfig: stageCfg, RootData: stageData}
	rep, err := readAndCheck(ctx, path, opts.Keys, opts.Checks, roots)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{Report: rep}
	if !rep.OK() {
		return res, fmt.Errorf("backup: the backup failed verification; nothing was changed")
	}
	if len(rep.Newer) > 0 && !opts.Force && !opts.DryRun {
		return res, ErrNewer
	}

	// Secrets: harden the restored ones, or carry the live ones over when the
	// backup has none.
	for _, e := range rep.Manifest.Entries {
		if e.Kind != KindSecret {
			continue
		}
		root, rel, _ := strings.Cut(e.Path, "/")
		if err := secrets.Harden(filepath.Join(roots[root], filepath.FromSlash(rel))); err != nil {
			return res, fmt.Errorf("backup: protect restored secret %s: %w", e.Path, err)
		}
	}
	if !rep.Manifest.Secrets {
		kept, err := carrySecrets(opts.Secrets, cfg, data, roots)
		if err != nil {
			return res, err
		}
		res.KeptSecrets = kept
	}
	if opts.DryRun {
		return res, nil
	}

	swaps := []*dirSwap{{target: data, staged: stageData, aside: data + ".pre-restore-" + stamp}}
	if !shared {
		swaps = append([]*dirSwap{{target: cfg, staged: stageCfg, aside: cfg + ".pre-restore-" + stamp}}, swaps...)
	}
	// Windows cannot rename a directory while a file inside it is open, and
	// the lock file is one; any other process still inside makes the rename
	// fail, which stops the restore just the same. Elsewhere the lock is held
	// through the renames.
	if runtime.GOOS == "windows" {
		_ = lock.Release()
	}
	if err := swapDirs(swaps); err != nil {
		return res, err
	}
	restored = true
	res.Restored = true
	for _, s := range swaps {
		if s.movedAside {
			res.Aside = append(res.Aside, s.aside)
		}
	}
	return res, nil
}

// carrySecrets copies each live secret file that the restored tree lacks into
// it, at the same place relative to its root, owner-only.
func carrySecrets(live []string, cfg, data string, roots map[string]string) ([]string, error) {
	var kept []string
	for _, p := range live {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		var root, base string
		switch {
		case within(abs, cfg) && !samePath(cfg, data):
			root, base = RootConfig, cfg
		case within(abs, data):
			root, base = RootData, data
		default:
			continue
		}
		rel, err := filepath.Rel(base, abs)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		body, err := secrets.Peek(abs)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return kept, fmt.Errorf("backup: read live secret %s: %w", abs, err)
		}
		// When one directory holds both, it is staged once; either root
		// name resolves to it.
		dst := filepath.Join(roots[root], rel)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := secrets.WriteFile(dst, body); err != nil {
			return kept, fmt.Errorf("backup: carry over %s: %w", abs, err)
		}
		kept = append(kept, root+"/"+filepath.ToSlash(rel))
	}
	return kept, nil
}
