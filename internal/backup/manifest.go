// Package backup writes, verifies and restores one consistent snapshot of
// Tenant's config and data directories (OPS-002 / TEN-305).
//
// A backup is a tar stream, gzip-compressed and optionally age-encrypted. Its
// first entry is MANIFEST.json, which lists every other entry in order with
// its size, SHA-256 and kind. A reader checks each entry against the manifest as it streams,
// so a truncated, reordered, extended or altered backup fails before anything
// is restored.
package backup

import (
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// FormatVersion is the backup layout this binary writes. A reader refuses a
// backup with a newer format.
const FormatVersion = 1

// manifestName is the first entry of every backup.
const manifestName = "MANIFEST.json"

// maxManifestBytes bounds the manifest a reader will decode.
const maxManifestBytes = 16 << 20

// Kind says how an entry was captured and how it is checked.
type Kind string

const (
	// KindSQLite is a SQLite database captured with VACUUM INTO. Verify runs
	// PRAGMA integrity_check on it and compares its tables and columns with
	// the ones this build of Tenant creates.
	KindSQLite Kind = "sqlite"
	// KindLog is an append-only JSONL log cut at its last complete record.
	// Verify parses every line.
	KindLog Kind = "log"
	// KindSecret is a credential or key file. Secrets are only written into
	// encrypted backups and are restored owner-only.
	KindSecret Kind = "secret"
	// KindFile is any other file, copied as it is.
	KindFile Kind = "file"
)

// The two roots a backup covers. Every entry path starts with one of them.
const (
	RootConfig = "config"
	RootData   = "data"
)

// Manifest describes a backup. It travels inside the (optionally encrypted)
// stream, so nothing in it is visible without the key.
type Manifest struct {
	Format        int       `json:"format"`
	TenantVersion string    `json:"tenant_version"`
	CreatedAt     time.Time `json:"created_at"`
	// Platform is the GOOS/GOARCH the backup was made on.
	Platform  string  `json:"platform"`
	Encrypted bool    `json:"encrypted"`
	Secrets   bool    `json:"secrets"`
	Entries   []Entry `json:"entries"`
	// Skipped lists files left out on purpose, with the reason, so a restore
	// can say what it will not bring back.
	Skipped []Skipped `json:"skipped,omitempty"`
}

// Entry is one file in a backup.
type Entry struct {
	// Path is slash-separated and starts with "config/" or "data/".
	Path   string `json:"path"`
	Kind   Kind   `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Skipped is a file a backup deliberately does not carry.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// validate checks a decoded manifest before any entry is read.
func (m *Manifest) validate() error {
	if m.Format < 1 {
		return fmt.Errorf("backup: manifest has no format version")
	}
	if m.Format > FormatVersion {
		return fmt.Errorf("backup: format %d is newer than this Tenant supports (%d); upgrade Tenant to read it", m.Format, FormatVersion)
	}
	seen := make(map[string]bool, len(m.Entries))
	folded := make(map[string]string, len(m.Entries))
	for i, e := range m.Entries {
		if err := validEntryPath(e.Path); err != nil {
			return fmt.Errorf("backup: entry %d: %w", i, err)
		}
		if seen[e.Path] {
			return fmt.Errorf("backup: entry %q appears twice", e.Path)
		}
		seen[e.Path] = true
		// Windows and macOS fold case, so two names that differ only in case
		// would restore onto one file.
		key := strings.ToLower(e.Path)
		if other, ok := folded[key]; ok {
			return fmt.Errorf("backup: entries %q and %q differ only in case", other, e.Path)
		}
		folded[key] = e.Path
		switch e.Kind {
		case KindSQLite, KindLog, KindFile:
		case KindSecret:
			if !m.Secrets {
				return fmt.Errorf("backup: entry %q is a secret but the manifest says secrets are not included", e.Path)
			}
		default:
			return fmt.Errorf("backup: entry %q has unknown kind %q", e.Path, e.Kind)
		}
		if e.Size < 0 {
			return fmt.Errorf("backup: entry %q has a negative size", e.Path)
		}
		if len(e.SHA256) != 64 {
			return fmt.Errorf("backup: entry %q has a malformed SHA-256", e.Path)
		}
		if _, err := hex.DecodeString(e.SHA256); err != nil {
			return fmt.Errorf("backup: entry %q has a malformed SHA-256", e.Path)
		}
	}
	return nil
}

// validEntryPath accepts only a clean, relative, slash-separated path under
// one of the two roots, whose every element is a plain name on this OS. It is
// the traversal guard for restore: nothing it accepts can land outside the
// root it names. A clean path has no empty, "." or ".." element below its
// root, since path.Clean removes them.
func validEntryPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsAny(p, "\\:\x00") {
		return fmt.Errorf("path %q contains a backslash, colon or NUL", p)
	}
	if path.Clean(p) != p || path.IsAbs(p) {
		return fmt.Errorf("path %q is not clean and relative", p)
	}
	root, rest, ok := strings.Cut(p, "/")
	if !ok || rest == "" {
		return fmt.Errorf("path %q names no file under a root", p)
	}
	if root != RootConfig && root != RootData {
		return fmt.Errorf("path %q is outside the config and data roots", p)
	}
	if !filepath.IsLocal(filepath.FromSlash(rest)) {
		return fmt.Errorf("path %q is not a local name on this system", p)
	}
	return nil
}

// Count returns how many entries are of kind k.
func (m *Manifest) Count(k Kind) int {
	n := 0
	for _, e := range m.Entries {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// TotalSize is the sum of every entry's size.
func (m *Manifest) TotalSize() int64 {
	var n int64
	for _, e := range m.Entries {
		n += e.Size
	}
	return n
}
