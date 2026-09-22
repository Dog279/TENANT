package sandbox

import (
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
				for _, f := range []string{"--die-with-parent", "--new-session"} {
					if !contains(argv, f) {
						t.Errorf("missing %s: %v", f, argv)
					}
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
			},
		},
		{
			name:  "NetAllowlist does not unshare net (out-of-band; TODO)",
			pol:   Policy{WorkDir: "/w", Network: NetAllowlist, Allow: []string{"example.com"}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				if contains(argv, "--unshare-net") {
					t.Errorf("NetAllowlist must not fully cut the network: %v", argv)
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
			pol:   Policy{WorkDir: "/w", DenyRoots: []string{"/etc/secret", "/var/keys"}},
			shell: "sh",
			args:  []string{"-c", "true"},
			check: func(t *testing.T, argv []string) {
				for _, dr := range []string{"/etc/secret", "/var/keys"} {
					if !pairAt(argv, "--tmpfs", dr) {
						t.Errorf("DenyRoot %s must be tmpfs-masked: %v", dr, argv)
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
