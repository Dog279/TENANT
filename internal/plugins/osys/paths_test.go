package osys

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testRoots(t *testing.T) (Roots, string, string) {
	t.Helper()
	base := t.TempDir()
	allowed := filepath.Join(base, "tenant")
	denied := filepath.Join(base, "windows")
	for _, d := range []string{allowed, denied} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return Roots{Deny: []string{denied}, Write: []string{allowed}}, allowed, denied
}

// A denied root is refused for READS too - the point is that the directory is
// invisible, not merely read-only.
func TestRoots_DenyBlocksReadAndWrite(t *testing.T) {
	r, _, denied := testRoots(t)
	target := filepath.Join(denied, "secret.txt")

	for _, write := range []bool{false, true} {
		err := r.Check(target, write)
		if err == nil {
			t.Fatalf("write=%v: denied root must be refused", write)
		}
		if !strings.Contains(err.Error(), "protected directory") {
			t.Errorf("write=%v: unhelpful error: %v", write, err)
		}
		// A hard boundary must say so, otherwise the agent retries via approval.
		if !strings.Contains(err.Error(), "not approvable") {
			t.Errorf("write=%v: refusal should state it is not approvable: %v", write, err)
		}
	}
}

func TestRoots_WriteConfinedButReadsBroad(t *testing.T) {
	r, allowed, _ := testRoots(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")

	if err := r.Check(filepath.Join(allowed, "notes.md"), true); err != nil {
		t.Errorf("write inside an allowed root must pass: %v", err)
	}
	if err := r.Check(outside, true); err == nil {
		t.Error("write outside the write roots must be refused")
	} else if !strings.Contains(err.Error(), "outside the writable roots") {
		t.Errorf("unhelpful error: %v", err)
	}
	// Reads outside Write (but outside Deny too) stay allowed so diagnostics work.
	if err := r.Check(outside, false); err != nil {
		t.Errorf("read outside the write roots should still be allowed: %v", err)
	}
}

// The traversal case: a path that LOOKS like it is inside the allowed root but
// climbs out of it. Refusing this is the whole reason paths are resolved before
// comparison rather than string-prefixed.
func TestRoots_RefusesDotDotEscape(t *testing.T) {
	r, allowed, denied := testRoots(t)
	escape := filepath.Join(allowed, "..", filepath.Base(denied), "pwned.txt")

	if err := r.Check(escape, true); err == nil {
		t.Fatalf("%q climbs into the denied root and must be refused", escape)
	}
}

// A file that does not exist yet must still be checked against its RESOLVED
// parent - otherwise every create bypasses the boundary.
func TestRoots_ChecksNonexistentPathsViaParent(t *testing.T) {
	r, allowed, denied := testRoots(t)

	if err := r.Check(filepath.Join(allowed, "sub", "deep", "new.txt"), true); err != nil {
		t.Errorf("a new file under an allowed root must be permitted: %v", err)
	}
	if err := r.Check(filepath.Join(denied, "sub", "new.txt"), true); err == nil {
		t.Error("a new file under a denied root must be refused")
	}
}

// A symlinked parent pointing out of the allowed tree must not launder a write.
func TestRoots_RefusesSymlinkEscape(t *testing.T) {
	r, allowed, denied := testRoots(t)
	link := filepath.Join(allowed, "backdoor")
	if err := os.Symlink(denied, link); err != nil {
		t.Skipf("symlinks unavailable on this host/user: %v", err)
	}
	if err := r.Check(filepath.Join(link, "pwned.txt"), true); err == nil {
		t.Fatal("a symlink into a denied root must be refused")
	}
}

// Windows paths are case-insensitive; C:\TENANT and c:\tenant are one place.
func TestRoots_CaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only semantics")
	}
	r, _, denied := testRoots(t)
	if err := r.Check(strings.ToUpper(filepath.Join(denied, "x.txt")), false); err == nil {
		t.Error("case variation must not bypass a denied root")
	}
}

// Empty roots preserve the historical unrestricted behavior, so existing
// callers that never configure roots are unaffected.
func TestRoots_EmptyIsUnrestricted(t *testing.T) {
	var r Roots
	if err := r.Check(filepath.Join(t.TempDir(), "anything.txt"), true); err != nil {
		t.Errorf("unconfigured roots must not restrict: %v", err)
	}
}

// A dangling symlink is followed to its target: EvalSymlinks gives up on one,
// but creating a file through it creates the target, which here sits in a
// denied root.
func TestRoots_RefusesDanglingSymlinkEscape(t *testing.T) {
	r, allowed, denied := testRoots(t)
	link := filepath.Join(allowed, "innocent.txt")
	if err := os.Symlink(filepath.Join(denied, "created-through-link.txt"), link); err != nil {
		t.Skipf("symlinks unavailable on this host/user: %v", err)
	}
	if err := r.Check(link, true); err == nil || !strings.Contains(err.Error(), "protected directory") {
		t.Fatalf("a dangling symlink into a denied root must be refused, got %v", err)
	}
}

// A symlink cycle cannot be resolved, so the path is refused, not waved through.
func TestRoots_RefusesSymlinkLoop(t *testing.T) {
	r, allowed, _ := testRoots(t)
	a, b := filepath.Join(allowed, "a"), filepath.Join(allowed, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlinks unavailable on this host/user: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(filepath.Join(a, "x.txt"), true); err == nil {
		t.Fatal("a path through a symlink loop must be refused")
	}
}

// protectRoots is a Tenant-shaped layout: a writable base holding a config dir
// whose settings file (settings.<agent>.json) and config.json are protected.
func protectRoots(t *testing.T) (r Roots, base, cfg, settings string) {
	t.Helper()
	base = t.TempDir()
	cfg = filepath.Join(base, "cfg")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	settings = filepath.Join(cfg, "settings.main.json")
	for _, f := range []string{settings, filepath.Join(cfg, "config.json")} {
		if err := os.WriteFile(f, []byte(`{"permissions":{"write":"ask"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r = Roots{
		Write:   []string{base},
		Protect: []string{filepath.Join(cfg, "settings.*.json"), filepath.Join(cfg, "config.json")},
	}
	return r, base, cfg, settings
}

func wantHardRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: a write to a protected file must be refused", what)
		return
	}
	for _, want := range []string{"protected file", "hard boundary", "not approvable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: refusal should say %q: %v", what, want, err)
		}
	}
}

// Every spelling of a protected file's path resolves to the file, so every one
// is refused: absolute, relative to the working directory, "..", through a
// symlinked directory, through a dangling symlink, beneath it, by case, and
// for an agent whose settings file does not exist yet.
func TestRoots_ProtectRefusesEveryPathToTheFile(t *testing.T) {
	r, base, cfg, settings := protectRoots(t)
	if err := os.MkdirAll(filepath.Join(base, "other"), 0o755); err != nil {
		t.Fatal(err)
	}

	paths := map[string]string{
		"absolute":            settings,
		"dot-dot":             filepath.Join(base, "other", "..", "cfg", "settings.main.json"),
		"case variant":        filepath.Join(cfg, "SETTINGS.Main.JSON"),
		"case variant, new":   filepath.Join(cfg, "SETTINGS.Other.JSON"), // name folding alone must catch it
		"long-s spelling":     filepath.Join(cfg, "ſettings.main.json"),
		"another agent":       filepath.Join(cfg, "settings.new-agent.json"),
		"beneath (as a dir)":  filepath.Join(cfg, "settings.main.json", "x"),
		"exact-name entry":    filepath.Join(cfg, "config.json"),
		"dot-dot, exact name": filepath.Join(cfg, "..", "cfg", "config.json"),
	}
	if err := os.Symlink(cfg, filepath.Join(base, "cfglink")); err == nil {
		paths["symlinked dir"] = filepath.Join(base, "cfglink", "settings.main.json")
		// Dangling: settings.later.json does not exist, so EvalSymlinks gives up.
		dangling := filepath.Join(base, "innocent.json")
		if err := os.Symlink(filepath.Join(cfg, "settings.later.json"), dangling); err != nil {
			t.Fatal(err)
		}
		paths["dangling symlink"] = dangling
	} else {
		t.Logf("symlinks unavailable on this host/user, skipping those cases: %v", err)
	}
	for what, p := range paths {
		wantHardRefusal(t, what, r.Check(p, true))
	}

	t.Chdir(cfg)
	wantHardRefusal(t, "relative to the working directory", r.Check("settings.main.json", true))
	t.Chdir(base)
	wantHardRefusal(t, "relative with a dir", r.Check(filepath.Join("cfg", "settings.main.json"), true))
}

// A hard link is the same file under an unrelated name, so name matching alone
// would miss it; the file's identity does not.
func TestRoots_ProtectRefusesHardLink(t *testing.T) {
	r, base, _, settings := protectRoots(t)
	alias := filepath.Join(base, "alias.json")
	if err := os.Link(settings, alias); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	wantHardRefusal(t, "hard link", r.Check(alias, true))
}

// Protection is for writes: the agent may still read the file, and ordinary
// files beside it, including near-miss names, stay writable.
func TestRoots_ProtectLeavesReadsAndNeighbours(t *testing.T) {
	r, base, cfg, settings := protectRoots(t)
	if err := r.Check(settings, false); err != nil {
		t.Errorf("reading a protected file must stay allowed: %v", err)
	}
	for _, p := range []string{
		filepath.Join(cfg, "soul.main.toml"),
		filepath.Join(cfg, "notes", "settings.main.json"), // same name, different dir
		filepath.Join(cfg, "settings.main.json.bak"),
		filepath.Join(base, "settings.main.json"),
	} {
		if err := r.Check(p, true); err != nil {
			t.Errorf("%s is not protected and must stay writable: %v", p, err)
		}
	}
}

// With no Write roots (unrestricted writes, as `tenant os` runs), Protect still
// applies.
func TestRoots_ProtectAppliesWithoutWriteRoots(t *testing.T) {
	r, _, _, settings := protectRoots(t)
	r.Write = nil
	wantHardRefusal(t, "no write roots", r.Check(settings, true))
}

// macOS reaches the data volume a second way, through its firmlinks:
// /System/Volumes/Data/private/... is /private/.... No symlink is involved, so
// for a file that does not exist yet only the directory's identity shows that
// the path lands in the protected directory.
func TestRoots_ProtectSeesThroughFirmlinks(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS firmlinks")
	}
	r, _, cfg, _ := protectRoots(t)
	r.Write = nil // no write roots to refuse the alias on other grounds
	real, err := filepath.EvalSymlinks(cfg)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join("/System/Volumes/Data", real)
	a, aerr := os.Stat(alias)
	b, berr := os.Stat(real)
	if aerr != nil || berr != nil || !os.SameFile(a, b) {
		t.Skipf("no firmlinked data volume here (%v, %v)", aerr, berr)
	}
	wantHardRefusal(t, "firmlinked dir, new file", r.Check(filepath.Join(alias, "settings.other.json"), true))
}

// secretRoots is a layout with a secret file (credentials.json in a config
// dir) and a secret directory (a home dir's .ssh), inside a writable base.
func secretRoots(t *testing.T) (r Roots, base, creds, key string) {
	t.Helper()
	base = t.TempDir()
	cfg, ssh := filepath.Join(base, "cfg"), filepath.Join(base, "home", ".ssh")
	for _, d := range []string{cfg, ssh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	creds, key = filepath.Join(cfg, "credentials.json"), filepath.Join(ssh, "id_ed25519")
	for _, f := range []string{creds, key} {
		if err := os.WriteFile(f, []byte("s3cret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Roots{Write: []string{base}, Secret: []string{creds, ssh}}, base, creds, key
}

func wantSecretRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: a secret must be refused", what)
		return
	}
	for _, want := range []string{"holds secrets", "hard boundary", "not approvable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: refusal should say %q: %v", what, want, err)
		}
	}
}

// A secret is refused to reads as well as writes, by every path to it: the
// file itself, anything in a secret directory and the directory itself, by
// case, relative to the working directory, through "..", a symlinked dir or a
// dangling symlink, and a hard link to it or to a file directly inside a secret
// directory.
func TestRoots_SecretRefusesReadsAndWrites(t *testing.T) {
	r, base, creds, key := secretRoots(t)
	cfg, ssh := filepath.Dir(creds), filepath.Dir(key)
	paths := map[string]string{
		"secret file":             creds,
		"file in a secret dir":    key,
		"the secret dir":          ssh,
		"new file in secret dir":  filepath.Join(ssh, "authorized_keys"),
		"case variant":            filepath.Join(cfg, "Credentials.JSON"),
		"dot-dot":                 filepath.Join(base, "home", "..", "cfg", "credentials.json"),
		"dot-dot into secret dir": filepath.Join(ssh, "..", ".ssh", "config"),
	}
	if err := os.Link(creds, filepath.Join(base, "creds-alias")); err == nil {
		paths["hard link to a secret file"] = filepath.Join(base, "creds-alias")
	}
	if err := os.Link(key, filepath.Join(base, "key-alias")); err == nil {
		paths["hard link into a secret dir"] = filepath.Join(base, "key-alias")
	}
	if err := os.Symlink(cfg, filepath.Join(base, "cfglink")); err == nil {
		paths["symlinked dir"] = filepath.Join(base, "cfglink", "credentials.json")
		if err := os.Symlink(filepath.Join(ssh, "new_key"), filepath.Join(base, "innocent")); err != nil {
			t.Fatal(err)
		}
		paths["dangling symlink into a secret dir"] = filepath.Join(base, "innocent")
	} else {
		t.Logf("symlinks unavailable on this host/user, skipping those cases: %v", err)
	}
	for what, p := range paths {
		for _, write := range []bool{false, true} {
			wantSecretRefusal(t, fmt.Sprintf("%s (write=%v)", what, write), r.Check(p, write))
		}
	}
	t.Chdir(cfg)
	wantSecretRefusal(t, "relative to the working directory", r.Check("credentials.json", false))

	// Beside the secrets, everything stays readable and writable.
	for _, p := range []string{filepath.Join(cfg, "config.toml"), filepath.Join(base, "home", ".sshrc"), filepath.Join(base, "home", "notes.md")} {
		for _, write := range []bool{false, true} {
			if err := r.Check(p, write); err != nil {
				t.Errorf("%s (write=%v) is not a secret: %v", p, write, err)
			}
		}
	}
}

// The identity half of the race closure. It is what refuses a redirected open
// where openFile cannot refuse the symlink (Windows, an older kernel): the
// open file alone gives away a Protect or Secret file, or a file directly
// inside a Secret directory. Reading a Protect file stays allowed.
func TestRoots_CheckOpenedRecognisesByIdentity(t *testing.T) {
	pr, _, _, settings := protectRoots(t)
	sr, base, creds, key := secretRoots(t)
	r := Roots{Protect: pr.Protect, Secret: sr.Secret}
	stat := func(p string) fs.FileInfo {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	if err := r.checkOpened(stat(settings)); err == nil || !strings.Contains(err.Error(), "protected file") {
		t.Errorf("an open Protect file must be refused to writes: %v", err)
	}
	if err := r.checkOpenedRead(stat(settings)); err != nil {
		t.Errorf("an open Protect file may be read: %v", err)
	}
	for _, p := range []string{creds, key} {
		for name, check := range map[string]func(fs.FileInfo) error{"write": r.checkOpened, "read": r.checkOpenedRead} {
			if err := check(stat(p)); err == nil || !strings.Contains(err.Error(), "holds secrets") {
				t.Errorf("an open secret (%s) must be refused to a %s: %v", filepath.Base(p), name, err)
			}
		}
	}
	ordinary := filepath.Join(base, "notes.md")
	if err := os.WriteFile(ordinary, []byte("s3cret"), 0o644); err != nil { // same size as the secrets
		t.Fatal(err)
	}
	if err := r.checkOpened(stat(ordinary)); err != nil {
		t.Errorf("an ordinary file is not guarded: %v", err)
	}
}

// Operator-supplied paths are literal: only "*" is a wildcard, so a "[" in a
// file name names exactly that file (filepath.Match would have read "[prod]"
// as a character class matching "sap.json").
func TestRoots_EntriesAreLiteralApartFromStar(t *testing.T) {
	base := t.TempDir()
	key := filepath.Join(base, "sa[prod].json")
	r := Roots{Secret: []string{key}}
	wantSecretRefusal(t, "the literal name", r.Check(key, false))
	for _, p := range []string{filepath.Join(base, "sap.json"), filepath.Join(base, "sa?prod?.json")} {
		if err := r.Check(p, false); err != nil {
			t.Errorf("%s is a different file and must stay readable: %v", p, err)
		}
	}
}

func TestWildcardMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"settings.*.json", "settings.main.json", true},
		{"settings.*.json", "settings..json", true},
		{"settings.*.json", "settings.json", false}, // "*" cannot make prefix and suffix overlap
		{"settings.*.json", "settings.main.json.tmp", false},
		{"settings.*.json.tmp", "settings.main.json.tmp", true},
		{"a*b*c", "a-x-b-y-c", true},
		{"a*b*c", "a-c-b", false},
		{"sa[prod].json", "sa[prod].json", true},
		{"sa[prod].json", "sap.json", false},
		{"*", "", true},
	} {
		if got := wildcardMatch(c.pattern, c.name); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// NTFS opens settings.json for "settings.json." and "settings.json::$DATA".
func TestRoots_ProtectWindowsNameForms(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only semantics")
	}
	r, _, _, settings := protectRoots(t)
	for _, p := range []string{settings + ".", settings + " ", settings + "::$DATA"} {
		wantHardRefusal(t, p, r.Check(p, true))
	}
}

func TestDefaultDeny_CoversTheDangerousPlaces(t *testing.T) {
	d := DefaultDeny()
	if len(d) == 0 {
		t.Fatal("default deny list is empty")
	}
	joined := strings.ToLower(strings.Join(d, "|"))
	want := "windows"
	if runtime.GOOS != "windows" {
		want = "/etc"
	}
	if !strings.Contains(joined, want) {
		t.Errorf("default deny should cover %q, got %v", want, d)
	}
}
