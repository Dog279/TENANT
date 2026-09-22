package sandbox

// spec.go holds the pure, OS-agnostic builders that turn a Policy into the
// low-level argument vectors / limit structs the OS backends hand to the
// kernel. Everything here is string- and value-shuffling only: NO syscalls, NO
// build tags, NO golang.org/x/sys types. That is what lets these builders be
// unit-tested on any host (see spec_test.go) even though the backends that
// consume them (restricted_linux.go, restricted_windows.go) only build and run
// on their own OS. The one exception is bwrapMask's os.Stat: whether a deny root
// is a directory, a file, or absent decides how bubblewrap can hide it.

import (
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// Linux: bubblewrap (bwrap) argument-vector builder.
// ---------------------------------------------------------------------------

// bwrapArgs builds the bubblewrap argument vector — everything AFTER the "bwrap"
// program name — for running `shell args...` under Policy pol. restricted_linux.go
// passes the result straight to exec.CommandContext(ctx, "bwrap", bwrapArgs(...)...).
//
// Order matters: bubblewrap applies filesystem operations left-to-right and a
// later op wins over an earlier one for overlapping paths. The layout is:
//
//  1. Isolation flags — --die-with-parent, --new-session and namespace
//     unshares; --unshare-net is added iff the policy forbids the network.
//  2. Read layer — with empty ReadRoots, --ro-bind / / makes the whole host
//     readable; with ReadRoots set, only linuxSystemReadRoots plus the read
//     roots are bound (an allowlist). Then a minimal --dev/--proc and an
//     ephemeral --tmpfs /tmp.
//  3. Write layer — a read-write --bind for WorkDir and each WriteRoot.
//  4. Deny layer — a --tmpfs mask per DenyRoot, applied LAST so a deny always
//     wins over a read/write bind (Policy: DenyRoots is checked first and wins).
//  5. --chdir WorkDir, then the "--" separator and the command.
//
// A host-filtered network allowlist is not implemented: NetAllowlist fails
// closed (the network is unshared). TODO(TEN-346): Landlock, rlimits.
func bwrapArgs(pol Policy, shell string, args []string) []string {
	a := make([]string, 0, 32+2*(len(pol.WriteRoots)+len(pol.DenyRoots))+len(args))

	// 1. Base isolation.
	a = append(a,
		"--die-with-parent", // tear the sandbox down if Tenant exits
		"--new-session",     // detach the controlling TTY (blocks TIOCSTI injection)
		"--unshare-pid",     // fresh PID namespace
		"--unshare-ipc",     // fresh SysV IPC namespace
		"--unshare-uts",     // fresh UTS (hostname) namespace
	)
	if pol.Network == NetNone || pol.Network == NetAllowlist {
		// NetAllowlist has no bubblewrap-native mechanism and nothing else enforces
		// it yet, so it fails CLOSED: no network rather than all of it. NetInherit
		// and NetFull share the host network.
		a = append(a, "--unshare-net")
	}

	// 2. Read layer. Empty ReadRoots (Policy: whole host readable): the host root
	// read-only. Non-empty ReadRoots are an ALLOWLIST: only the system directories
	// a shell and its tools need plus the read roots are bound, so everything else
	// (a home dir, another project) does not exist inside the sandbox.
	// --ro-bind-try skips a source this distribution does not have.
	if len(pol.ReadRoots) == 0 {
		a = append(a, "--ro-bind", "/", "/")
	} else {
		for _, p := range linuxSystemReadRoots {
			a = append(a, "--ro-bind-try", p, p)
		}
		for _, r := range pol.ReadRoots {
			if r != "" {
				a = append(a, "--ro-bind-try", r, r)
			}
		}
	}
	a = append(a,
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	)

	// 3. Write layer: each WriteRoot becomes a read-write bind.
	for _, wr := range pol.WriteRoots {
		if wr == "" {
			continue
		}
		a = append(a, "--bind", wr, wr)
	}

	// 4. Deny layer after the grants so it overrides any read/write bind above. A
	// tmpfs mask makes the host path an empty, ephemeral dir: its real contents
	// are neither readable nor (persistently) writable.
	for _, dr := range pol.DenyRoots {
		a = append(a, bwrapMask(dr)...)
	}

	// 5. WorkDir carve-out (Policy precedence rule 3): bound AFTER the deny masks,
	// so a work dir inside a deny root (cron's, under the data dir) stays usable.
	// bwrap takes --bind SRC from the host and creates the mount point inside the
	// tmpfs. Then every deny root at or inside the work dir is masked again, so the
	// carve-out never reopens it.
	if pol.WorkDir != "" {
		a = append(a, "--bind", pol.WorkDir, pol.WorkDir)
		for _, dr := range pol.DenyRoots {
			if within(dr, pol.WorkDir) {
				a = append(a, bwrapMask(dr)...)
			}
		}
	}

	// 6. cwd inside the sandbox, then the command.
	if pol.WorkDir != "" {
		a = append(a, "--chdir", pol.WorkDir)
	}
	a = append(a, "--", shell)
	a = append(a, args...)
	return a
}

// ---------------------------------------------------------------------------
// Windows: Job Object limit mapping (pure).
// ---------------------------------------------------------------------------

// Win32 JOB_OBJECT_LIMIT_* bit values (winnt.h). They live in this pure file so
// jobLimitsFrom is testable off-Windows; restricted_windows.go carries a
// compile-time assertion that each still equals its golang.org/x/sys/windows
// counterpart, so the two can never silently drift.
const (
	jobLimitProcessTime   uint32 = 0x00000002 // JOB_OBJECT_LIMIT_PROCESS_TIME
	jobLimitActiveProcess uint32 = 0x00000008 // JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	jobLimitJobMemory     uint32 = 0x00000200 // JOB_OBJECT_LIMIT_JOB_MEMORY
	jobLimitKillOnClose   uint32 = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
)

// jobLimits is the OS-agnostic projection of a Policy's Limits onto a Windows
// Job Object's JOBOBJECT_EXTENDED_LIMIT_INFORMATION. It is pure Go (no syscalls,
// no golang.org/x/sys types) so the mapping is unit-tested on any host;
// restricted_windows.go copies these fields into the real windows struct.
type jobLimits struct {
	Flags                   uint32 // OR of the jobLimit* bits above
	ActiveProcessLimit      uint32 // JOB_OBJECT_LIMIT_ACTIVE_PROCESS count; 0 ⇒ unused
	JobMemoryLimit          uint64 // JOB_OBJECT_LIMIT_JOB_MEMORY bytes;     0 ⇒ unused
	PerProcessUserTime100ns int64  // JOB_OBJECT_LIMIT_PROCESS_TIME in 100ns ticks; 0 ⇒ unused
}

// jobLimitsFrom maps a Limits onto the Job Object fields. KILL_ON_JOB_CLOSE is
// ALWAYS set so closing (or killing) the job tears the whole process tree down;
// the other three bits are set only when the matching Limits field is non-zero.
func jobLimitsFrom(l Limits) jobLimits {
	jl := jobLimits{Flags: jobLimitKillOnClose}
	if l.MaxPIDs > 0 {
		jl.Flags |= jobLimitActiveProcess
		jl.ActiveProcessLimit = uint32(l.MaxPIDs)
	}
	if l.MemoryBytes > 0 {
		jl.Flags |= jobLimitJobMemory
		jl.JobMemoryLimit = uint64(l.MemoryBytes)
	}
	if l.CPU > 0 {
		jl.Flags |= jobLimitProcessTime
		// A Job Object's PerProcessUserTimeLimit is counted in 100-nanosecond ticks.
		jl.PerProcessUserTime100ns = l.CPU.Nanoseconds() / 100
	}
	return jl
}

// bwrapMask hides one deny root from the sandbox. A directory gets an empty tmpfs;
// a file gets /dev/null bound over it (tmpfs cannot mount on a file). A path that
// does not exist is skipped: there is nothing to hide, writes are allow-only
// anyway, and bwrap cannot create the mount point under the read-only root, so
// masking it would fail the whole run (the credential-store deny list is mostly
// paths that are absent on any given host).
func bwrapMask(path string) []string {
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return nil
	case fi.IsDir():
		return []string{"--tmpfs", path}
	default:
		return []string{"--ro-bind", "/dev/null", path}
	}
}

// linuxSystemReadRoots are what /bin/sh and common tools need to run when
// ReadRoots is an allowlist. /etc is included for the loader cache, passwd and
// TLS roots (its secrets, /etc/shadow, are root-only anyway).
var linuxSystemReadRoots = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/run/systemd/resolve"}

// attributeBwrapDenial makes a best-effort Violation from a failed bwrap run, so
// refusals escalate (and dry-run works) on Linux as they do on macOS. A write
// outside the write roots hits the read-only host mount (EROFS); an outbound
// connect with the network unshared fails as unreachable or unresolvable. A read
// of a masked deny root looks exactly like a missing file (ENOENT), so reads are
// not attributed: those refusals fail the command without escalating.
func attributeBwrapDenial(output string, pol Policy) (Violation, bool) {
	low := strings.ToLower(output)
	if strings.Contains(low, "read-only file system") {
		return Violation{Kind: "filesystem", Detail: "sandbox blocked a write outside the policy's write roots (read-only)"}, true
	}
	if pol.Network == NetNone || pol.Network == NetAllowlist {
		for _, sig := range []string{"network is unreachable", "temporary failure in name resolution",
			"could not resolve host", "name or service not known", "no address associated with hostname"} {
			if strings.Contains(low, sig) {
				return Violation{Kind: "network", Detail: "sandbox blocked a network operation (network disabled for this profile)"}, true
			}
		}
	}
	return Violation{}, false
}
