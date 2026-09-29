package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- small argv assertion helpers ---

func contains(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}

// pairAt reports whether flag appears immediately followed by a.
func pairAt(argv []string, flag, a string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == a {
			return true
		}
	}
	return false
}

// tripleAt reports whether flag appears immediately followed by a then b.
func tripleAt(argv []string, flag, a, b string) bool {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == a && argv[i+2] == b {
			return true
		}
	}
	return false
}

// index is where seq first appears in argv, or -1.
func index(argv []string, seq ...string) int {
	for i := 0; i+len(seq) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(seq)], seq) {
			return i
		}
	}
	return -1
}

func endsWith(argv, tail []string) bool {
	if len(tail) > len(argv) {
		return false
	}
	off := len(argv) - len(tail)
	for i, t := range tail {
		if argv[off+i] != t {
			return false
		}
	}
	return true
}

func TestBwrapArgs(t *testing.T) {
	// Deny masks depend on what is on disk (dir → tmpfs, file → /dev/null bind,
	// absent → skipped), so deny-root cases use real paths.
	secretDir, keysDir := t.TempDir(), t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "id_key")
	if err := os.WriteFile(keyFile, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	tests := []struct {
		name  string
		pol   Policy
		shell string
		args  []string
		check func(t *testing.T, argv []string)
	}{
		{
			name:  "core flags, workdir bind/chdir and command tail",
			pol:   Policy{WorkDir: "/w"},
			shell: "sh",
			args:  []string{"-c", "echo hi"},
			check: func(t *testing.T, argv []string) {
				if !tripleAt(argv, "--ro-bind", "/", "/") {
					t.Errorf("missing --ro-bind / /: %v", argv)
				}
				if !tripleAt(argv, "--bind", "/w", "/w") {
					t.Errorf("missing rw --bind for WorkDir: %v", argv)
				}
				if !pairAt(argv, "--tmpfs", "/tmp") {
					t.Errorf("missing --tmpfs /tmp: %v", argv)
				}
				if !pairAt(argv, "--chdir", "/w") {
					t.Errorf("missing --chdir /w: %v", argv)
				}
				for _, f := range []string{"--die-with-parent", "--new-session", "--unshare-pid"} {
					if !contains(argv, f) {
						t.Errorf("missing %s: %v", f, argv)
					}
				}
				// bwrap run as root keeps every capability unless told otherwise.
				if !pairAt(argv, "--cap-drop", "ALL") {
					t.Errorf("missing --cap-drop ALL: %v", argv)
				}
				if contains(argv, "--unshare-net") {
					t.Errorf("NetInherit must not unshare net: %v", argv)
				}
				if !endsWith(argv, []string{"--", "sh", "-c", "echo hi"}) {
					t.Errorf("argv must end with -- shell args, got tail %v", argv)
				}
			},
		},
		{
			name:  "NetNone unshares the network",
			pol:   Policy{WorkDir: "/w", Network: NetNone},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				if !contains(argv, "--unshare-net") {
					t.Errorf("NetNone must add --unshare-net: %v", argv)
				}
				for _, f := range linuxResolvConfs {
					if contains(argv, f) {
						t.Errorf("with the network cut the resolver files stay hidden (%s): %v", f, argv)
					}
				}
			},
		},
		{
			name:  "the host readable: /run hidden after the root bind, resolver files back with the network shared",
			pol:   Policy{WorkDir: "/w", Network: NetFull},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				root, run := index(argv, "--ro-bind", "/", "/"), index(argv, "--tmpfs", "/run")
				if root < 0 || run < root {
					t.Errorf("/run must be masked after --ro-bind / / (root at %d, mask at %d): %v", root, run, argv)
				}
				stub := "/run/systemd/resolve/stub-resolv.conf"
				back := index(argv, "--ro-bind-try", stub, stub)
				if back < run {
					t.Errorf("the resolver file must be bound back after the /run mask: %v", argv)
				}
				if index(argv, "--remount-ro", "/run") < back || pairAt(argv, "--remount-ro", "/") {
					t.Errorf("/run (not the ro-bound root) goes read-only after the bind-back: %v", argv)
				}
			},
		},
		{
			name:  "NetAllowlist fails CLOSED (nothing enforces an allowlist yet)",
			pol:   Policy{WorkDir: "/w", Network: NetAllowlist, Allow: []string{"example.com"}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				if !contains(argv, "--unshare-net") {
					t.Errorf("NetAllowlist must cut the network rather than grant all of it: %v", argv)
				}
			},
		},
		{
			name:  "non-empty ReadRoots is an allowlist: system dirs + read roots, never the whole host",
			pol:   Policy{WorkDir: "/w", ReadRoots: []string{"/proj"}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				if tripleAt(argv, "--ro-bind", "/", "/") {
					t.Errorf("ReadRoots set: the whole host must not be bound: %v", argv)
				}
				for _, want := range []string{"/usr", "/etc", "/proj"} {
					if !tripleAt(argv, "--ro-bind-try", want, want) {
						t.Errorf("allowlist must bind %s: %v", want, argv)
					}
				}
				// systemd-resolved's directory holds its socket: never bound whole.
				if contains(argv, "/run/systemd/resolve") {
					t.Errorf("allowlist must not bind the resolver's directory: %v", argv)
				}
				// bwrap's own root is a tmpfs: read-only once the last bind is in.
				if index(argv, "--remount-ro", "/") < index(argv, "--bind", "/w", "/w") {
					t.Errorf("the root tmpfs must be remounted read-only after the binds: %v", argv)
				}
			},
		},
		{
			name:  "empty ReadRoots binds the whole host read-only",
			pol:   Policy{WorkDir: "/w"},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				if !tripleAt(argv, "--ro-bind", "/", "/") {
					t.Errorf("empty ReadRoots means whole host readable: %v", argv)
				}
			},
		},
		{
			name:  "NetFull does not unshare net",
			pol:   Policy{WorkDir: "/w", Network: NetFull},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				if contains(argv, "--unshare-net") {
					t.Errorf("NetFull must not unshare net: %v", argv)
				}
			},
		},
		{
			name:  "each DenyRoot is masked with its own --tmpfs",
			pol:   Policy{WorkDir: "/w", DenyRoots: []string{secretDir, keysDir}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				for _, dr := range []string{secretDir, keysDir} {
					if !pairAt(argv, "--tmpfs", dr) {
						t.Errorf("DenyRoot %s must be tmpfs-masked: %v", dr, argv)
					}
					// Read-only, so a write into it fails instead of landing in a throwaway tmpfs.
					if index(argv, "--remount-ro", dr) < index(argv, "--tmpfs", dr) {
						t.Errorf("DenyRoot %s must be remounted read-only after its mask: %v", dr, argv)
					}
				}
			},
		},
		{
			name:  "a file DenyRoot is hidden with /dev/null (tmpfs cannot mount on a file)",
			pol:   Policy{WorkDir: "/w", DenyRoots: []string{keyFile}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				if !tripleAt(argv, "--ro-bind", "/dev/null", keyFile) || pairAt(argv, "--tmpfs", keyFile) {
					t.Errorf("file deny root must be /dev/null-bound, never tmpfs: %v", argv)
				}
				if pairAt(argv, "--remount-ro", keyFile) {
					t.Errorf("a file mask is a read-only bind already; --remount-ro is for tmpfs masks: %v", argv)
				}
			},
		},
		{
			name:  "an absent DenyRoot is skipped (bwrap cannot mkdir under the ro root)",
			pol:   Policy{WorkDir: "/w", DenyRoots: []string{missing}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				for _, a := range argv {
					if a == missing {
						t.Errorf("absent deny root must not appear (it would fail the run): %v", argv)
					}
				}
			},
		},
		{
			name:  "each WriteRoot gets a rw --bind",
			pol:   Policy{WorkDir: "/w", WriteRoots: []string{"/data/out", "/cache"}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				for _, wr := range []string{"/data/out", "/cache"} {
					if !tripleAt(argv, "--bind", wr, wr) {
						t.Errorf("WriteRoot %s must be rw-bound: %v", wr, argv)
					}
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			argv := bwrapArgs(tc.pol, tc.shell, tc.args)
			tc.check(t, argv)
		})
	}
}

func TestJobLimitsFrom(t *testing.T) {
	// The pure bit values must match winnt.h (restricted_windows.go additionally
	// asserts they equal golang.org/x/sys/windows' constants at compile time).
	if jobLimitProcessTime != 0x2 || jobLimitActiveProcess != 0x8 ||
		jobLimitJobMemory != 0x200 || jobLimitKillOnClose != 0x2000 {
		t.Fatalf("JOB_OBJECT_LIMIT_* bit values drifted from winnt.h")
	}

	// Zero limits: kill-on-close only, all value fields unset.
	jl := jobLimitsFrom(Limits{})
	if jl.Flags != jobLimitKillOnClose {
		t.Fatalf("zero limits flags = %#x, want kill-on-close %#x", jl.Flags, jobLimitKillOnClose)
	}
	if jl.ActiveProcessLimit != 0 || jl.JobMemoryLimit != 0 || jl.PerProcessUserTime100ns != 0 {
		t.Fatalf("zero limits must leave value fields unset: %+v", jl)
	}

	// Populated limits: every bit set and every field mapped.
	jl = jobLimitsFrom(Limits{MemoryBytes: 1 << 30, MaxPIDs: 8, CPU: 2 * time.Second})
	if jl.Flags&jobLimitKillOnClose == 0 {
		t.Errorf("kill-on-close must always be set: %#x", jl.Flags)
	}
	if jl.Flags&jobLimitJobMemory == 0 || jl.JobMemoryLimit != 1<<30 {
		t.Errorf("memory mapping wrong: %+v", jl)
	}
	if jl.Flags&jobLimitActiveProcess == 0 || jl.ActiveProcessLimit != 8 {
		t.Errorf("pid mapping wrong: %+v", jl)
	}
	wantTicks := int64(2*time.Second) / 100 // 100ns ticks
	if jl.Flags&jobLimitProcessTime == 0 || jl.PerProcessUserTime100ns != wantTicks {
		t.Errorf("cpu mapping wrong: got %d want %d", jl.PerProcessUserTime100ns, wantTicks)
	}
}

// A failed bubblewrap self-check names the host setting that blocks it.
func TestUsernsHint(t *testing.T) {
	read := func(files map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v + "\n"), nil
			}
			return nil, errors.New("absent")
		}
	}
	const aa = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	cases := []struct {
		files map[string]string
		want  string // substring; "" = no hint
	}{
		{map[string]string{aa: "1"}, "apparmor_restrict_unprivileged_userns=1"},
		{map[string]string{aa: "0", "/proc/sys/user/max_user_namespaces": "0"}, "max_user_namespaces=0"},
		{map[string]string{"/proc/sys/kernel/unprivileged_userns_clone": "0"}, "unprivileged_userns_clone=0"},
		{map[string]string{aa: "0", "/proc/sys/user/max_user_namespaces": "63000"}, ""},
		{nil, ""}, // not Linux, or no such settings
	}
	for _, c := range cases {
		got := usernsHint(read(c.files))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("usernsHint(%v) = %q, want %q", c.files, got, c.want)
		}
	}
}

// Linux refusals must be attributed so escalation and dry-run work there too.
func TestAttributeBwrapDenial(t *testing.T) {
	none := Policy{Network: NetNone}
	full := Policy{Network: NetFull}
	cases := []struct {
		name, out string
		pol       Policy
		kind      string
	}{
		{"write outside the write roots", "sh: 1: cannot create /home/u/x: Read-only file system", full, "filesystem"},
		{"connect with the network unshared", "curl: (7) Couldn't connect to server: Network is unreachable", none, "network"},
		{"dns with the network unshared", "curl: (6) Could not resolve host: example.com", none, "network"},
		{"dns failure on a profile WITH network is not a refusal", "curl: (6) Could not resolve host: example.com", full, ""},
		{"a missing file is not attributable (a masked root looks the same)", "cat: /x: No such file or directory", none, ""},
	}
	for _, c := range cases {
		v, ok := attributeBwrapDenial(c.out, c.pol)
		if c.kind == "" {
			if ok {
				t.Errorf("%s: must not be attributed, got %+v", c.name, v)
			}
			continue
		}
		if !ok || v.Kind != c.kind {
			t.Errorf("%s: want kind %q, got %+v ok=%v", c.name, c.kind, v, ok)
		}
	}
}
