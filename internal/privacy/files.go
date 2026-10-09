package privacy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Directories of the file-based classes inside the data dir.
const (
	researchDir    = "research"
	screenshotsDir = "screenshots"
	relayInboxDir  = "relay-inbox"
	evalDir        = "eval-artifacts"
	// attachmentsDir holds the images messages carry (TEN-493), in YYYY-MM
	// month folders. They follow the Archive class: archive lines name them.
	attachmentsDir = "attachments"
)

// selectAttachments adds the attachment files older than before, and the
// month folders named in months, so images go when the archive lines that
// name them do.
func (s *selection) selectAttachments(d Data, before time.Time, months map[string]bool) error {
	entries, err := os.ReadDir(d.path(attachmentsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, m := range entries {
		if !m.IsDir() {
			continue
		}
		if months[m.Name()] {
			s.paths[d.path(attachmentsDir, m.Name())] = true
			continue
		}
		if !before.IsZero() {
			if err := s.addOld(d.path(attachmentsDir, m.Name()), before, false, regularFile); err != nil {
				return err
			}
		}
	}
	return nil
}

// selectFilesBefore adds the files and directories of each class that are
// older than its cutoff: research runs by the time in their name, the rest by
// modification time. Eval baselines and the trend log are never selected.
func (s *selection) selectFilesBefore(d Data, cutoffs map[Class]time.Time) error {
	for class, cutoff := range cutoffs {
		if cutoff.IsZero() {
			continue
		}
		var err error
		switch class {
		case Research:
			err = s.addOld(d.path(researchDir), cutoff, true, func(e fs.DirEntry) (time.Time, bool) {
				if !e.IsDir() {
					return time.Time{}, false
				}
				if len(e.Name()) >= 15 {
					if t, perr := time.ParseInLocation("20060102-150405", e.Name()[:15], time.Local); perr == nil {
						return t, true
					}
				}
				return modTime(e)
			})
		case Screenshots:
			err = s.addOld(d.path(screenshotsDir), cutoff, false, regularFile)
		case EvalArtifacts:
			err = s.addOld(d.path(evalDir), cutoff, false, func(e fs.DirEntry) (time.Time, bool) {
				if !strings.HasPrefix(e.Name(), "eval-") || !strings.HasSuffix(e.Name(), ".json") {
					return time.Time{}, false
				}
				return regularFile(e)
			})
		case RelayInbox:
			days, rerr := os.ReadDir(d.path(relayInboxDir))
			if errors.Is(rerr, fs.ErrNotExist) {
				continue
			}
			if rerr != nil {
				return rerr
			}
			for _, day := range days {
				if day.IsDir() {
					if err = s.addOld(d.path(relayInboxDir, day.Name()), cutoff, false, regularFile); err != nil {
						return err
					}
				}
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// addOld adds the entries of dir that when() dates before cutoff.
func (s *selection) addOld(dir string, cutoff time.Time, dirs bool, when func(fs.DirEntry) (time.Time, bool)) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() != dirs {
			continue
		}
		if t, ok := when(e); ok && t.Before(cutoff) {
			s.paths[filepath.Join(dir, e.Name())] = true
		}
	}
	return nil
}

func regularFile(e fs.DirEntry) (time.Time, bool) {
	if !e.Type().IsRegular() {
		return time.Time{}, false
	}
	return modTime(e)
}

func modTime(e fs.DirEntry) (time.Time, bool) {
	info, err := e.Info()
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// pathsByClass groups selected paths for the preview.
func pathsByClass(d Data, paths map[string]bool) map[Class][]string {
	out := map[Class][]string{}
	for p := range paths {
		rel, err := filepath.Rel(d.DataDir, p)
		if err != nil {
			continue
		}
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		switch top {
		case researchDir:
			out[Research] = append(out[Research], p)
		case screenshotsDir:
			out[Screenshots] = append(out[Screenshots], p)
		case relayInboxDir:
			out[RelayInbox] = append(out[RelayInbox], p)
		case attachmentsDir:
			out[Archive] = append(out[Archive], p)
		case evalDir:
			out[EvalArtifacts] = append(out[EvalArtifacts], p)
		}
	}
	for c := range out {
		sort.Strings(out[c])
	}
	return out
}

// removePaths deletes the selected files and directories, then any relay
// inbox day directory left empty.
func removePaths(d Data, paths map[string]bool) (int, error) {
	n := 0
	for _, p := range sortedKeys(paths) {
		if err := os.RemoveAll(p); err != nil {
			return n, err
		}
		n++
	}
	for _, dir := range []string{relayInboxDir, attachmentsDir} {
		days, err := os.ReadDir(d.path(dir))
		if err == nil {
			for _, day := range days {
				if day.IsDir() {
					_ = os.Remove(d.path(dir, day.Name())) // only succeeds when empty
				}
			}
		}
	}
	return n, nil
}
