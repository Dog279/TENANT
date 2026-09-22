package sandbox

// spec.go holds the pure, OS-agnostic builders that turn a Policy into the
// low-level argument vectors / limit structs the OS backends hand to the
// kernel. Everything here is string- and value-shuffling only: NO syscalls, NO
// build tags, NO golang.org/x/sys types. That is what lets these builders be
// unit-tested on any host (see spec_test.go) even though the backends that
// consume them (restricted_linux.go, restricted_windows.go) only build and run
// on their own OS.

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
//  2. Read layer — --ro-bind / / makes the whole host readable (Policy: empty
//     ReadRoots ⇒ whole host readable), plus a minimal --dev/--proc and an
//     ephemeral --tmpfs /tmp.
//  3. Write layer — a read-write --bind for WorkDir and each WriteRoot.
//  4. Deny layer — a --tmpfs mask per DenyRoot, applied LAST so a deny always
//     wins over a read/write bind (Policy: DenyRoots is checked first and wins).
//  5. --chdir WorkDir, then the "--" separator and the command.
//
// Tightening on top of this (honouring a non-empty ReadRoots as a strict
// allowlist, and NetAllowlist via nftables/proxy) is TODO(TEN-346).
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
	if pol.Network == NetNone {
		a = append(a, "--unshare-net") // no outbound network at all
	}
	// NetAllowlist has no bubblewrap-native mechanism; it is enforced out of band
	// (nftables/proxy) — TODO(TEN-346). NetInherit and NetFull share the host net.

	// 2. Read layer: whole host read-only, minimal /dev + /proc, ephemeral /tmp.
	a = append(a,
		"--ro-bind", "/", "/",
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
		if dr == "" {
			continue
		}
		a = append(a, "--tmpfs", dr)
	}

	// 5. WorkDir carve-out (Policy precedence rule 3): bound AFTER the deny masks,
	// so a work dir inside a deny root (cron's, under the data dir) stays usable.
	// bwrap takes --bind SRC from the host and creates the mount point inside the
	// tmpfs. Then every deny root at or inside the work dir is masked again, so the
	// carve-out never reopens it.
	if pol.WorkDir != "" {
		a = append(a, "--bind", pol.WorkDir, pol.WorkDir)
		for _, dr := range pol.DenyRoots {
			if dr != "" && within(dr, pol.WorkDir) {
				a = append(a, "--tmpfs", dr)
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
