package sandbox

// spec.go holds the pure, OS-agnostic builders that turn a Policy into the
// low-level argument vectors / limit structs the OS backends hand to the
// kernel. Everything here is string- and value-shuffling only: NO syscalls, NO
// build tags, NO golang.org/x/sys types. That is what lets these builders be
// unit-tested on any host (see spec_test.go) even though the backends that
// consume them (restricted_linux.go, restricted_windows.go) only build and run
// on their own OS. The one exception is bwrapMask's stat: whether a deny root
// is a directory, a file, or absent decides how bubblewrap can hide it. A
// backend that translates paths (WSL) passes a stat that looks at the path it
// translated from, since the translated one does not exist where Tenant runs.

import (
	"os"
	"strings"
)

// statFunc reports what a path is, as the confining side sees it.
type statFunc func(string) (os.FileInfo, error)

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
//  1. Isolation flags — --die-with-parent, --new-session, --cap-drop ALL and
//     namespace unshares; --unshare-net is added iff the policy forbids the network.
//  2. Read layer — with empty ReadRoots, --ro-bind / / makes the whole host
//     readable but /run; with ReadRoots set, only linuxSystemReadRoots plus the
//     read roots are bound (an allowlist). A shared network gets the resolver
//     files back. Then a minimal --dev/--proc and an ephemeral --tmpfs /tmp.
//  3. Write layer — a read-write --bind for each WriteRoot.
//  4. Deny layer — a mask per DenyRoot, after the grants so a deny always wins
//     over a read/write bind (Policy: DenyRoots is checked first and wins).
//  5. WorkDir carve-out, then every tmpfs layer but /tmp is remounted read-only.
//  6. --chdir WorkDir, then the "--" separator and the command.
//
// A host-filtered network allowlist is not implemented: NetAllowlist fails
// closed (the network is unshared). Resource caps are not bubblewrap's job:
// bwrapLimitArgv wraps the command before it gets here.
func bwrapArgs(pol Policy, shell string, args []string) []string {
	return bwrapArgsOpts(pol, bwrapOpts{stat: os.Stat}, shell, args)
}

// bwrapOpts are what a backend that runs bubblewrap somewhere other than
// where Tenant runs (WSL) must say about that place.
type bwrapOpts struct {
	// stat classifies each deny root for bwrapMask.
	stat statFunc
	// hide lists trees to hide when the host is readable (empty ReadRoots):
	// bound back only where the work dir and the write roots are. WSL hides
	// the Windows drives, whose case-insensitive names defeat a deny mask.
	hide []string
}

// bwrapArgsOpts is bwrapArgs for a backend with bwrapOpts.
func bwrapArgsOpts(pol Policy, opts bwrapOpts, shell string, args []string) []string {
	stat := opts.stat
	if stat == nil {
		stat = os.Stat
	}
	a := make([]string, 0, 32+2*(len(pol.WriteRoots)+len(pol.DenyRoots))+len(args))

	// 1. Base isolation.
	a = append(a,
		"--die-with-parent", // tear the sandbox down if Tenant exits
		"--new-session",     // detach the controlling TTY (blocks TIOCSTI injection)
		// Unprivileged bwrap drops every capability anyway; run as root it keeps
		// them all, and the command could unmount a deny mask to read under it.
		"--cap-drop", "ALL",
		"--unshare-pid", // fresh PID namespace: what the command leaves running dies with it
		"--unshare-ipc", // fresh SysV IPC namespace
		"--unshare-uts", // fresh UTS (hostname) namespace
	)
	netCut := pol.Network == NetNone || pol.Network == NetAllowlist
	if netCut {
		// NetAllowlist has no bubblewrap-native mechanism and nothing else enforces
		// it yet, so it fails CLOSED: no network rather than all of it. NetInherit
		// and NetFull share the host network.
		a = append(a, "--unshare-net")
	}

	// 2. Read layer. Empty ReadRoots (Policy: whole host readable): the host root
	// read-only, but for /run. The host's daemons listen there (the D-Bus and
	// systemd buses, through which systemd-run starts a process outside any
	// sandbox; container runtimes; WSL interop), and a unix socket stays
	// connectable through a read-only mount and across network namespaces.
	// Non-empty ReadRoots are an ALLOWLIST: only the system directories a shell
	// and its tools need plus the read roots are bound, so everything else (a
	// home dir, another project) does not exist inside the sandbox.
	// --ro-bind-try skips a source this distribution does not have.
	var ro []string // tmpfs layers, remounted read-only at the end of step 5
	if len(pol.ReadRoots) == 0 {
		a = append(a, "--ro-bind", "/", "/", "--tmpfs", "/run")
		ro = append(ro, "/run")
		for _, h := range opts.hide {
			a = append(a, "--tmpfs", h)
			ro = append(ro, h)
		}
	} else {
		ro = append(ro, "/") // bwrap's own root, a tmpfs holding the binds' mount points
		for _, p := range linuxSystemReadRoots {
			a = append(a, "--ro-bind-try", p, p)
		}
		for _, r := range pol.ReadRoots {
			if r != "" {
				a = append(a, "--ro-bind-try", r, r)
			}
		}
	}
	if !netCut {
		// DNS: /etc/resolv.conf often links into /run. The files only, never the
		// resolver's sockets beside them.
		for _, f := range linuxResolvConfs {
			a = append(a, "--ro-bind-try", f, f)
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
	// tmpfs mask makes the host path an empty dir: its real contents are neither
	// readable nor writable.
	for _, dr := range pol.DenyRoots {
		m := bwrapMask(dr, stat)
		if len(m) > 0 && m[0] == "--tmpfs" {
			ro = append(ro, dr)
		}
		a = append(a, m...)
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
				a = append(a, bwrapMask(dr, stat)...)
			}
		}
	}
	// Every tmpfs layer but /tmp goes read-only only now, once each bind has made
	// its mount point in one (bwrap cannot in a read-only one). A write outside
	// the grants, a deny root included, then fails (EROFS, attributed as a
	// refusal) instead of landing in a throwaway layer. Each bind is a mount of
	// its own and stays as it was.
	for _, p := range ro {
		a = append(a, "--remount-ro", p)
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

// bwrapMask hides one deny root from the sandbox. A directory gets an empty tmpfs
// (remounted read-only once the carve-out is in); a file gets /dev/null bound
// over it (tmpfs cannot mount on a file), so a write to it is discarded. A path that
// does not exist is skipped: there is nothing to hide, writes are allow-only
// anyway, and bwrap cannot create the mount point under the read-only root, so
// masking it would fail the whole run (the credential-store deny list is mostly
// paths that are absent on any given host).
func bwrapMask(path string, stat statFunc) []string {
	if path == "" {
		return nil
	}
	fi, err := stat(path)
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
var linuxSystemReadRoots = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc"}

// linuxResolvConfs are where /etc/resolv.conf links when the resolver lives in
// /run (systemd-resolved, resolvconf, NetworkManager). They are bound back,
// as files, only when the sandbox shares the host network: with it cut they
// are useless, and systemd-resolved's socket beside them would still resolve
// names for the sandbox, a DNS channel out.
var linuxResolvConfs = []string{
	"/run/systemd/resolve/stub-resolv.conf",
	"/run/systemd/resolve/resolv.conf",
	"/run/resolvconf/resolv.conf",
	"/run/NetworkManager/resolv.conf",
	"/run/NetworkManager/no-stub-resolv.conf",
}

// usernsHint names the host setting that stops an unprivileged bubblewrap
// creating its user namespace, read through read (os.ReadFile), for the error
// a failed self-check reports. "" when none of them is set.
func usernsHint(read func(string) ([]byte, error)) string {
	for _, s := range []struct{ file, val, say string }{
		{"/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1",
			"AppArmor restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns=1, " +
				"the Ubuntu 23.10+ default): give bwrap an AppArmor profile that allows userns, or set it to 0"},
		{"/proc/sys/kernel/unprivileged_userns_clone", "0",
			"unprivileged user namespaces are disabled (kernel.unprivileged_userns_clone=0)"},
		{"/proc/sys/user/max_user_namespaces", "0",
			"user namespaces are disabled (user.max_user_namespaces=0)"},
	} {
		if b, err := read(s.file); err == nil && strings.TrimSpace(string(b)) == s.val {
			return s.say
		}
	}
	return ""
}

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
