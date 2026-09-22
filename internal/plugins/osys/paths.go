package osys

// paths.go confines the file tools to operator-chosen roots.
//
// This is a REAL boundary for the file tools (os_read_file, os_write_file,
// os_append_file, os_edit_file, os_make_dir, os_list_dir, os_read_image): every
// path is resolved to an absolute, symlink-evaluated form BEFORE the check, so
// "C:\Tenant\..\Windows\System32" and a symlink pointing out of the allowed
// tree are both refused. The tools then open exactly the path that was checked,
// refusing a symlink anywhere on it (openFile), and the Protect and Secret
// files are checked once more by identity after the open. So a symlink swapped
// in after the check cannot redirect the operation.
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
// Protect lists files no write may create, change or replace, even inside a
// Write root: the controls a write-enabled agent must not be able to hand
// itself, such as the file holding its own permission modes. Secret lists
// files no tool may read, change or replace: credentials. Both are hard
// boundaries no approval opens, and unlike Deny they are matched robustly: by
// case-folded name, by directory identity and, for a file that exists, by
// file identity, so no other spelling of or link to the same file gets
// through. An entry naming a directory covers everything in it. Its last
// element may use "*" as a wildcard ("settings.*.json"); every other
// character is literal.
//
// Empty lists mean "no restriction", which preserves the historical behavior
// for callers that do not configure roots.
type Roots struct {
	Deny    []string
	Write   []string
	Protect []string
	Secret  []string
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

// Check validates one path for a read (write=false) or a write; see Resolve.
func (r Roots) Check(p string, write bool) error {
	_, err := r.Resolve(p, write)
	return err
}

// Resolve validates one path and returns the absolute, symlink-free path it
// checked. That is the path the tools must open, with openFile: opening any
// other, the path as given included, would let a symlink swapped in afterwards
// redirect the operation. Deny and Secret refuse reads and writes; a write is
// also refused for a Protect file and outside the Write roots. The returned
// error is written for the MODEL: it names the boundary and the allowed roots,
// so the agent reports a scope problem instead of retrying the same refused
// path.
func (r Roots) Resolve(p string, write bool) (string, error) {
	target, err := resolve(p)
	if err != nil {
		return "", err
	}
	cmp := normalizeForCompare(target)

	for _, d := range r.Deny {
		dr, derr := resolve(d)
		if derr != nil {
			continue
		}
		if under(cmp, normalizeForCompare(dr)) {
			return "", fmt.Errorf("refused: %s is inside a protected directory (%s). "+
				"This is a hard boundary — it is not approvable", target, dr)
		}
	}
	if entry, ok := covering(r.Secret, target); ok {
		return "", secretErr(target, entry)
	}
	if !write {
		return target, nil
	}
	if entry, ok := covering(r.Protect, target); ok {
		return "", protectedErr(target, entry)
	}
	if len(r.Write) == 0 {
		return target, nil
	}
	for _, w := range r.Write {
		wr, werr := resolve(w)
		if werr != nil {
			continue
		}
		if under(cmp, normalizeForCompare(wr)) {
			return target, nil
		}
	}
	return "", fmt.Errorf("refused: %s is outside the writable roots. Tenant may write to: %s",
		target, strings.Join(r.Write, ", "))
}

// covering returns the entry that covers a resolved target, if any.
func covering(entries []string, target string) (string, bool) {
	if len(entries) == 0 {
		return "", false
	}
	var info fs.FileInfo // nil while the target does not exist yet
	if fi, err := os.Stat(target); err == nil {
		info = fi
	}
	for _, g := range resolveGuards(entries) {
		if g.covers(target, info) {
			return g.entry, true
		}
	}
	return "", false
}

// checkOpened refuses a file already opened for writing when it is a Protect
// or Secret file. Resolve vets the PATH before anything is opened; between the
// two, a process running alongside the turn (an os_exec child, a parallel tool
// call) could swap a symlink and re-point a vetted path. openFile makes such an
// open fail where the OS can refuse symlinks on the whole path; this catches a
// redirect by the open file's identity, which cannot be re-pointed, on every
// OS. The writing Service methods run it before they write a byte.
func (r Roots) checkOpened(info fs.FileInfo) error {
	if p, entry, ok := holding(r.Secret, info); ok {
		return secretErr(p, entry)
	}
	if p, entry, ok := holding(r.Protect, info); ok {
		return protectedErr(p, entry)
	}
	return nil
}

// checkOpenedRead is checkOpened for a file opened for reading, which only a
// Secret entry refuses.
func (r Roots) checkOpenedRead(info fs.FileInfo) error {
	if p, entry, ok := holding(r.Secret, info); ok {
		return secretErr(p, entry)
	}
	return nil
}

// holding reports which entry, if any, currently holds the file info
// describes, and under which path.
func holding(entries []string, info fs.FileInfo) (path, entry string, ok bool) {
	for _, g := range resolveGuards(entries) {
		if p, held := g.holds(info); held {
			return p, g.entry, true
		}
	}
	return "", "", false
}

func protectedErr(target, entry string) error {
	return fmt.Errorf("refused: %s is a protected file (%s) that the file tools may never write. "+
		"This is a hard boundary — it is not approvable, and any other path to the same file "+
		"is refused too. If it needs changing, ask the operator to do it", target, entry)
}

func secretErr(target, entry string) error {
	return fmt.Errorf("refused: %s holds secrets (%s) that the file tools may never read or write. "+
		"This is a hard boundary — it is not approvable, and any other path to the same file "+
		"is refused too. If you need something from it, ask the operator", target, entry)
}

// guard is one Protect or Secret entry, resolved for matching.
type guard struct {
	entry   string      // as configured, for the refusal message
	dir     string      // resolved directory
	pattern string      // the entry's last element, "*" wildcards allowed
	dirInfo fs.FileInfo // nil when the directory does not exist
}

func resolveGuards(entries []string) []guard {
	out := make([]guard, 0, len(entries))
	for _, e := range entries {
		d := filepath.Dir(e)
		dir, err := resolve(d)
		if err != nil {
			dir = filepath.Clean(d) // unresolvable: still match on the literal form
		}
		g := guard{entry: e, dir: dir, pattern: filepath.Base(e)}
		if fi, err := os.Stat(dir); err == nil {
			g.dirInfo = fi
		}
		out = append(out, g)
	}
	return out
}

// covers reports whether a resolved target is this entry, lies beneath it (a
// directory in a file's place breaks the file just as surely), or, when the
// target exists, is a file the entry holds under another name: a hard link, or
// a spelling the filesystem folds together that the name match missed.
func (g guard) covers(target string, info fs.FileInfo) bool {
	for a := target; ; {
		parent := filepath.Dir(a)
		if parent == a {
			break
		}
		if nameMatches(g.pattern, filepath.Base(a)) && g.isDir(parent) {
			return true
		}
		a = parent
	}
	if info == nil {
		return false
	}
	_, held := g.holds(info)
	return held
}

// isDir reports whether dir is the entry's directory: by folded name, or by
// identity when both exist, so neither a case variant nor a second path to the
// same directory (a macOS firmlink, a bind mount, a \\?\ prefix) hides it.
func (g guard) isDir(dir string) bool {
	if foldName(dir) == foldName(g.dir) {
		return true
	}
	if g.dirInfo == nil {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && os.SameFile(fi, g.dirInfo)
}

// holds reports whether info is a file the entry holds right now, and which:
// the file the entry names, or, when it names a directory, a file directly
// inside it. Deeper files are covered by path (covers) and by openFile.
func (g guard) holds(info fs.FileInfo) (string, bool) {
	if g.dirInfo == nil {
		return "", false
	}
	for _, p := range g.named() {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if os.SameFile(fi, info) {
			return p, true
		}
		if fi.IsDir() {
			if c, ok := childIs(p, info); ok {
				return c, true
			}
		}
	}
	return "", false
}

// named lists the paths the entry names in its directory: the one name, or
// every existing name its wildcard matches.
func (g guard) named() []string {
	if !strings.Contains(g.pattern, "*") {
		return []string{filepath.Join(g.dir, g.pattern)}
	}
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if nameMatches(g.pattern, e.Name()) {
			out = append(out, filepath.Join(g.dir, e.Name()))
		}
	}
	return out
}

// childIs reports which file directly inside dir, if any, is the file info
// describes. Sizes are compared first, so the identity check (which on
// Windows opens the file) runs only on real candidates.
func childIs(dir string, info fs.FileInfo) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ci, err := e.Info(); err != nil || ci.Size() != info.Size() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, info) {
			return p, true
		}
	}
	return "", false
}

// nameMatches reports whether a file name matches an entry's last element the
// way a case-insensitive filesystem (APFS and NTFS, by default) resolves
// names: letters fold, and on Windows a trailing dot or space and a ":stream"
// suffix are dropped, because there "settings.json." and "settings.json::$DATA"
// open settings.json itself. It errs toward matching: a false match only
// refuses an operation.
func nameMatches(pattern, name string) bool {
	if filepath.Separator == '\\' {
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		name = strings.TrimRight(name, ". ")
	}
	return wildcardMatch(foldName(pattern), foldName(name))
}

// wildcardMatch matches name against pattern, where "*" matches any run of
// characters and every other character is literal, so an operator path
// holding "[" or "?" still names exactly itself.
func wildcardMatch(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(name) < len(first)+len(last) || !strings.HasPrefix(name, first) || !strings.HasSuffix(name, last) {
		return false
	}
	name = name[len(first) : len(name)-len(last)]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(name, mid)
		if i < 0 {
			return false
		}
		name = name[i+len(mid):]
	}
	return true
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
