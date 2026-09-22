package osys

// paths.go confines the file tools to operator-chosen roots.
//
// This is a REAL boundary for the file tools (os_read_file, os_write_file,
// os_append_file, os_edit_file, os_make_dir, os_list_dir, os_read_image): every
// path is resolved to an absolute, symlink-evaluated form BEFORE the check, so
// "C:\Tenant\..\Windows\System32" and a symlink pointing out of the allowed
// tree are both refused. Protect files are checked a second time once the
// file is open (checkOpened), so a symlink swapped after the check cannot
// redirect a write into one.
//
// It is NOT a boundary for os_exec. A shell command is an opaque string; the
// danger classifier catches obvious destructive shapes but cannot bound where a
// command writes. The only real containment for exec is the OS: run Tenant as a
// least-privilege account whose ACLs deny the directories you care about. The
// package doc says the same thing, and it is worth repeating here because a
// configured WriteRoots list can otherwise read as stronger than it is.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Roots bounds where the file tools may operate.
//
// Deny is checked FIRST and applies to reads as well as writes, so a directory
// listed there is invisible to the agent no matter what else is configured.
// Write then restricts mutations; reads outside Write are still allowed (so
// diagnostics keep working) provided they are not under Deny.
//
// Protect lists individual FILES no write may create, change or replace, even
// inside a Write root: the controls a write-enabled agent must not be able to
// hand itself, such as the file holding its own permission modes. Like Deny it
// is a hard boundary no approval opens; unlike Deny it leaves the file
// readable. An entry's last element may be a filepath.Match pattern
// ("settings.*.json"); its directory is literal.
//
// Empty lists mean "no restriction", which preserves the historical behavior
// for callers that do not configure roots.
type Roots struct {
	Deny    []string
	Write   []string
	Protect []string
}

// maxLinkHops bounds how many dangling symlinks resolve follows by hand, so a
// link cycle cannot spin it forever. Matches Linux's MAXSYMLINKS.
const maxLinkHops = 40

// resolve normalizes a path for comparison: absolute, cleaned, and with
// symlinks evaluated where possible. A path that does not exist yet (a file
// about to be created) has its nearest existing ancestor evaluated instead, so
// a symlinked parent cannot be used to escape the allowed tree. A DANGLING
// symlink is followed by hand: EvalSymlinks gives up on one, but creating a
// file through it creates its target, so the target is what gets checked.
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", p, err)
	}
	abs = filepath.Clean(abs)
	probe := abs
	for hops := 0; ; {
		if real, rerr := filepath.EvalSymlinks(probe); rerr == nil {
			if probe == abs {
				return filepath.Clean(real), nil
			}
			// Re-attach the not-yet-existing tail to the resolved ancestor.
			rel, relErr := filepath.Rel(probe, abs)
			if relErr != nil {
				return abs, nil
			}
			return filepath.Clean(filepath.Join(real, rel)), nil
		}
		if fi, lerr := os.Lstat(probe); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			dest, rerr := os.Readlink(probe)
			if rerr != nil {
				return "", fmt.Errorf("cannot resolve %q: %w", p, rerr)
			}
			if hops++; hops > maxLinkHops {
				return "", fmt.Errorf("cannot resolve %q: too many levels of symbolic links", p)
			}
			if !filepath.IsAbs(dest) {
				dest = filepath.Join(filepath.Dir(probe), dest)
			}
			rel, relErr := filepath.Rel(probe, abs)
			if relErr != nil {
				return "", fmt.Errorf("cannot resolve %q: %w", p, relErr)
			}
			abs = filepath.Clean(filepath.Join(dest, rel))
			probe = abs
			continue
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// Nothing on the path exists; the cleaned absolute form is the best
			// available answer and still defeats plain ".." traversal.
			return abs, nil
		}
		probe = parent
	}
}

// under reports whether target is root itself or inside it. Compared
// case-insensitively on Windows, where C:\TENANT and c:\tenant are one path.
func under(target, root string) bool {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func normalizeForCompare(p string) string {
	if filepath.Separator == '\\' {
		return strings.ToLower(p)
	}
	return p
}

// Check validates one path. write=true additionally refuses a Protect file and
// requires the path to be inside a Write root. The returned error is written
// for the MODEL: it names the boundary and the allowed roots, so the agent
// reports a scope problem instead of retrying the same refused path.
func (r Roots) Check(p string, write bool) error {
	target, err := resolve(p)
	if err != nil {
		return err
	}
	cmp := normalizeForCompare(target)

	for _, d := range r.Deny {
		dr, derr := resolve(d)
		if derr != nil {
			continue
		}
		if under(cmp, normalizeForCompare(dr)) {
			return fmt.Errorf("refused: %s is inside a protected directory (%s). "+
				"This is a hard boundary — it is not approvable", target, dr)
		}
	}
	if !write {
		return nil
	}
	if err := r.checkProtected(target); err != nil {
		return err
	}
	if len(r.Write) == 0 {
		return nil
	}
	for _, w := range r.Write {
		wr, werr := resolve(w)
		if werr != nil {
			continue
		}
		if under(cmp, normalizeForCompare(wr)) {
			return nil
		}
	}
	return fmt.Errorf("refused: %s is outside the writable roots. Tenant may write to: %s",
		target, strings.Join(r.Write, ", "))
}

// checkProtected refuses a write whose resolved target is, or would replace, a
// Protect file.
func (r Roots) checkProtected(target string) error {
	if len(r.Protect) == 0 {
		return nil
	}
	var info fs.FileInfo // nil while the target does not exist yet
	if fi, err := os.Stat(target); err == nil {
		info = fi
	}
	for _, pf := range r.protectedFiles() {
		if pf.covers(target, info) {
			return protectedErr(target, pf.entry)
		}
	}
	return nil
}

// checkOpened refuses a file already opened for writing when it is one of the
// Protect files. Check vets the PATH before anything is opened; between the two,
// a process running alongside the turn (an os_exec child, a parallel tool call)
// could swap a symlink and re-point a vetted path at a protected file. An open
// file cannot be re-pointed, so comparing its identity closes that gap. The
// writing Service methods run it before they write a byte.
func (r Roots) checkOpened(info fs.FileInfo) error {
	for _, pf := range r.protectedFiles() {
		if p, ok := pf.existing(info); ok {
			return protectedErr(p, pf.entry)
		}
	}
	return nil
}

func protectedErr(target, entry string) error {
	return fmt.Errorf("refused: %s is a protected file (%s) that the file tools may never write. "+
		"This is a hard boundary — it is not approvable, and any other path to the same file "+
		"is refused too. If it needs changing, ask the operator to do it", target, entry)
}

// protectedFile is one Protect entry, resolved for matching.
type protectedFile struct {
	entry   string      // as configured, for the refusal message
	dir     string      // resolved directory
	pattern string      // file name, or a filepath.Match pattern for one
	dirInfo fs.FileInfo // nil when the directory does not exist
}

func (r Roots) protectedFiles() []protectedFile {
	out := make([]protectedFile, 0, len(r.Protect))
	for _, e := range r.Protect {
		d := filepath.Dir(e)
		dir, err := resolve(d)
		if err != nil {
			dir = filepath.Clean(d) // unresolvable: still match on the literal form
		}
		pf := protectedFile{entry: e, dir: dir, pattern: filepath.Base(e)}
		if fi, err := os.Stat(dir); err == nil {
			pf.dirInfo = fi
		}
		out = append(out, pf)
	}
	return out
}

// covers reports whether a resolved target is this protected file, lies beneath
// it (a directory of the same name breaks the file just as surely), or, when
// the target exists, is one of its files under another name: a hard link, or a
// spelling the filesystem folds together that the name match missed.
func (pf protectedFile) covers(target string, info fs.FileInfo) bool {
	for a := target; ; {
		parent := filepath.Dir(a)
		if parent == a {
			break
		}
		if nameMatches(pf.pattern, filepath.Base(a)) && pf.isDir(parent) {
			return true
		}
		a = parent
	}
	if info == nil {
		return false
	}
	_, same := pf.existing(info)
	return same
}

// isDir reports whether dir is the protected file's directory: by folded name,
// or by identity when both exist, so neither a case variant nor a second path
// to the same directory (a macOS firmlink, a bind mount, a \\?\ prefix) hides it.
func (pf protectedFile) isDir(dir string) bool {
	if foldName(dir) == foldName(pf.dir) {
		return true
	}
	if pf.dirInfo == nil {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && os.SameFile(fi, pf.dirInfo)
}

// existing reports whether info is one of the files that currently exist for
// this entry, and which one.
func (pf protectedFile) existing(info fs.FileInfo) (string, bool) {
	if pf.dirInfo == nil {
		return "", false
	}
	entries, err := os.ReadDir(pf.dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !nameMatches(pf.pattern, e.Name()) {
			continue
		}
		p := filepath.Join(pf.dir, e.Name())
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, info) {
			return p, true
		}
	}
	return "", false
}

// nameMatches reports whether a file name matches a Protect pattern the way a
// case-insensitive filesystem (APFS and NTFS, by default) resolves names:
// letters fold, and on Windows a trailing dot or space and a ":stream" suffix
// are dropped, because there "settings.json." and "settings.json::$DATA" open
// settings.json itself. It errs toward matching: a false match only refuses a
// write.
func nameMatches(pattern, name string) bool {
	if filepath.Separator == '\\' {
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		name = strings.TrimRight(name, ". ")
	}
	ok, err := filepath.Match(foldName(pattern), foldName(name))
	return err == nil && ok
}

// foldName maps every rune to the lowest member of its case-folding orbit, so
// names that differ only in case compare equal, including the long s ("ſ")
// and the Kelvin sign ("K") that case folding equates with s and k.
func foldName(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}

// DefaultDeny lists directories that should never be touched by an agent on
// this platform. Windows-first because that is where Tenant runs 24/7; the
// unix entries keep the behavior sane for dev machines.
func DefaultDeny() []string {
	if filepath.Separator == '\\' {
		sysRoot := os.Getenv("SystemRoot")
		if sysRoot == "" {
			sysRoot = `C:\Windows`
		}
		return []string{
			sysRoot,
			`C:\Program Files`,
			`C:\Program Files (x86)`,
			`C:\ProgramData`,
		}
	}
	return []string{"/etc", "/bin", "/sbin", "/usr/bin", "/usr/sbin", "/boot", "/sys", "/proc"}
}
