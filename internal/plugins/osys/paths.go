package osys

// paths.go confines the file tools to operator-chosen roots.
//
// This is a REAL boundary for the file tools (os_read_file, os_write_file,
// os_append_file, os_edit_file, os_make_dir, os_list_dir, os_read_image): every
// path is resolved to an absolute, symlink-evaluated form BEFORE the check, so
// "C:\Tenant\..\Windows\System32" and a symlink pointing out of the allowed
// tree are both refused.
//
// It is NOT a boundary for os_exec. A shell command is an opaque string; the
// danger classifier catches obvious destructive shapes but cannot bound where a
// command writes. The only real containment for exec is the OS: run Tenant as a
// least-privilege account whose ACLs deny the directories you care about. The
// package doc says the same thing, and it is worth repeating here because a
// configured WriteRoots list can otherwise read as stronger than it is.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Roots bounds where the file tools may operate.
//
// Deny is checked FIRST and applies to reads as well as writes, so a directory
// listed there is invisible to the agent no matter what else is configured.
// Write then restricts mutations; reads outside Write are still allowed (so
// diagnostics keep working) provided they are not under Deny.
//
// Empty lists mean "no restriction", which preserves the historical behavior
// for callers that do not configure roots.
type Roots struct {
	Deny  []string
	Write []string
}

// resolve normalizes a path for comparison: absolute, cleaned, and with
// symlinks evaluated where possible. A path that does not exist yet (a file
// about to be created) has its nearest existing ancestor evaluated instead, so
// a symlinked parent cannot be used to escape the allowed tree.
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", p, err)
	}
	abs = filepath.Clean(abs)
	probe := abs
	for {
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

// Check validates one path. write=true additionally requires the path to be
// inside a Write root. The returned error is written for the MODEL: it names
// the boundary and the allowed roots, so the agent reports a scope problem
// instead of retrying the same refused path.
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
	if !write || len(r.Write) == 0 {
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
