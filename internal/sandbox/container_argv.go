package sandbox

// container_argv.go builds the `docker run` argv from a fixed schema and holds
// the rules a golden test pins (W1-2 / H6). It has no side effects and touches
// no engine, so container_argv_test.go can assert the exact argv on any OS.
//
// The schema, flag for flag, is docs/plans/docker-sandbox-plan.md §3.3 plus the
// hardening the live probe motivated: --pull never, CSV-quoted --mount fields,
// --runtime runc, and the command as a single argv element after the image.
// Nothing here ever comes from model input, and no value is interpolated into a
// shell string — the OpenClaw CVE-2026-24763 lesson.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// mount is one bind mount: a canonical host source and the container path it is
// mounted at, read-only when ro.
type mount struct {
	host string // canonical host path (native form; never /mnt/... as a source)
	ctr  string // container destination
	ro   bool
}

// containerSpec is everything buildRunArgv needs. Every field is derived by the
// backend from trusted inputs (the policy, the command); none is passthrough.
type containerSpec struct {
	image    string
	name     string
	trace    string
	instance string
	work     mount
	mounts   []mount
	network  string // "none" or sandboxNetwork only (dockerNetwork maps the mode)
	envNames []string
	limits   Limits
	command  string
	// hostExists answers whether a host path exists, for ENOENT attribution
	// (attributeContainer); nil means "unknown", which never attributes.
	hostExists func(string) bool
	// witness is the streaming scan of the run's whole output, set by Run just
	// before attribution; nil (a spec built outside Run, as in tests) falls back
	// to scanning the capped Result.Output.
	witness *refusalWitness
}

// defaultLimits fill in a spec that set none, so a container is always capped
// even if a profile forgot to (fail closed on resources).
const (
	defaultMaxPIDs   = 256
	defaultMemoryMB  = 1024
	defaultCPUs      = "2"
	defaultTmpfsSize = "256m"

	// sandboxHome is HOME inside every container: a tmpfs of its own (D4), so
	// the dotfiles and caches a command writes die with the container. HOME
	// used to be the work dir, where they stayed on the host for the next
	// "fresh" container to read, and for host tools to find.
	sandboxHome     = "/home/sandbox"
	defaultHomeSize = "64m"
)

// deadlineGrace is added to the wall clock for the in-container deadline: the
// command is killed from inside (`timeout -s KILL`) a little after Tenant would
// have killed it from outside, so a container orphaned by a Tenant crash still
// dies on its own (W1-4 / H4). Tenant's own deadline fires first in the normal
// case, so the model's view of a timeout is unchanged.
const deadlineGrace = 5 * time.Second

// buildRunArgv renders the hardened `docker run` argv (everything after the
// binary). It validates every value that could carry an injection — mount
// fields, env names, the network — and returns an error rather than emit an
// argv that is not exactly the schema.
func buildRunArgv(s containerSpec) ([]string, error) {
	if s.image == "" {
		return nil, fmt.Errorf("no image")
	}
	if err := validImageRef(s.image); err != nil {
		return nil, err
	}
	if s.name == "" {
		return nil, fmt.Errorf("no container name")
	}
	if s.network != "none" && s.network != sandboxNetwork {
		return nil, fmt.Errorf("network %q is not none or %s", s.network, sandboxNetwork)
	}
	if s.work.host == "" || s.work.ctr == "" {
		return nil, fmt.Errorf("no work mount")
	}

	a := []string{"run", "--rm", "-i", "--pull", "never", "--name", s.name,
		"--label", labelSandbox + "=1"}
	if s.trace != "" {
		a = append(a, "--label", labelTrace+"="+labelValue(s.trace))
	}
	if s.instance != "" {
		a = append(a, "--label", labelInstance+"="+labelValue(s.instance))
	}

	// Read-only rootfs, a small nosuid tmpfs for /tmp, and one for HOME that
	// only the sandbox user can enter (the uid and gid are the --user below).
	a = append(a, "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size="+defaultTmpfsSize,
		"--tmpfs", sandboxHome+":rw,nosuid,nodev,size="+defaultHomeSize+",mode=0700,uid=65532,gid=65532")

	// The work mount (read-write) and the working directory.
	warg, err := mountArg(s.work)
	if err != nil {
		return nil, fmt.Errorf("work mount: %w", err)
	}
	a = append(a, "--mount", warg, "-w", s.work.ctr)

	for _, m := range s.mounts {
		marg, err := mountArg(m)
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.host, err)
		}
		a = append(a, "--mount", marg)
	}

	// Identity and privilege: non-root, no capabilities, no new privileges. These
	// are load-bearing together — the probe showed cap-drop without NNP still
	// reaches root through a setuid binary.
	a = append(a, "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges")

	// Pin the default OCI runtime: never the NVIDIA runtime, which would let an
	// image request a GPU on its own (NVIDIAScape, CVE-2025-23266).
	a = append(a, "--runtime", "runc")

	// Resource caps, with fail-closed defaults.
	pids := s.limits.MaxPIDs
	if pids <= 0 {
		pids = defaultMaxPIDs
	}
	memMB := s.limits.MemoryBytes >> 20
	if memMB <= 0 {
		memMB = defaultMemoryMB
	}
	mem := strconv.FormatInt(memMB, 10) + "m"
	a = append(a, "--pids-limit", strconv.Itoa(pids),
		"--memory", mem, "--memory-swap", mem, // no swap: memory-swap == memory
		"--cpus", defaultCPUs,
		"--ulimit", "core=0", "--ulimit", "nofile=1024:4096")
	// D5: a CPU-time cap is per process (RLIMIT_CPU), so it stops one runaway
	// process, not a command that spreads its work over many; the wall clock
	// bounds the whole run. That is why the backend reports Limits as partial.
	if s.limits.CPU > 0 {
		secs := strconv.FormatInt(int64((s.limits.CPU+time.Second-1)/time.Second), 10)
		a = append(a, "--ulimit", "cpu="+secs+":"+secs)
	}

	a = append(a, "--network", s.network, "--init", "--log-driver", "none")

	// Environment by name only: docker reads each value from the CLI's own
	// environment, so no value ever appears on the argv.
	for _, n := range s.envNames {
		if !validEnvName(n) {
			return nil, fmt.Errorf("invalid env name %q", n)
		}
		a = append(a, "-e", n)
	}
	a = append(a, "-e", "HOME="+sandboxHome, "-e", "TMPDIR=/tmp")

	// The image (pinned), then the command as ONE argv element after `sh -c`.
	// With a wall clock, coreutils' timeout wraps the shell inside the
	// container, so the command dies there even if nothing outside kills it.
	a = append(a, s.image)
	if s.limits.Wall > 0 {
		secs := int64((s.limits.Wall + deadlineGrace + time.Second - 1) / time.Second)
		a = append(a, "timeout", "-s", "KILL", strconv.FormatInt(secs, 10))
	}
	a = append(a, "sh", "-c", s.command)
	return a, nil
}

// mountArg renders a --mount value with CSV-quoted src and dst, and rejects a
// path with a character that could break out of the CSV field or inject another
// option (control characters, a quote, a newline) — the comma the probe hit is
// handled by the quoting (W1-2).
func mountArg(m mount) (string, error) {
	for _, p := range []string{m.host, m.ctr} {
		if strings.ContainsAny(p, "\x00\n\r") {
			return "", fmt.Errorf("path contains a control character: %q", p)
		}
	}
	// docker parses --mount with encoding/csv, splitting on commas into key=value
	// fields, so a whole `key=value` element is quoted when it contains a comma,
	// space or quote (the probe's comma path). type=bind has none, so it stays
	// bare.
	parts := []string{"type=bind", csvField("src=" + m.host), csvField("dst=" + m.ctr)}
	if m.ro {
		parts = append(parts, "readonly")
	}
	return strings.Join(parts, ","), nil
}

// csvField quotes a whole --mount element when it contains a comma, quote, or
// space, the way docker's CSV parser expects (a literal " is doubled). It does
// not quote on '=', because every element is a key=value pair.
func csvField(s string) string {
	if strings.ContainsAny(s, ",\" ") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// labelValue strips characters that would break a docker label so a trace or
// instance id is always a safe value.
func labelValue(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.' || r == ':':
			return r
		default:
			return '-'
		}
	}, s)
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

// validImageRef rejects an image reference that is not pinned enough to be safe:
// it must have a tag or a digest, and no character that could inject another
// argv element. A bare name (which docker would resolve to :latest and could
// pull) is allowed only as the built-in default tag; the backend also passes
// --pull never, so nothing reaches a registry regardless.
func validImageRef(ref string) error {
	if strings.ContainsAny(ref, " \t\n\r\"'") {
		return fmt.Errorf("image reference has whitespace or quotes: %q", ref)
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("image reference must not start with '-': %q", ref)
	}
	return nil
}

// forbiddenRunFlags are the flags buildRunArgv must never emit; the golden test
// asserts none of them appears in the argv, so a future edit that reintroduces
// one is caught (H6). They are the escape hatches: privilege, host namespaces,
// devices, the socket, an unpinned pull, a TTY (TIOCSTI), the NVIDIA runtime.
var forbiddenRunFlags = []string{
	"--privileged", "--privileged=true", "--cap-add",
	"--device", "--device-cgroup-rule", "--gpus",
	"--pid", "--pid=host", "--ipc", "--ipc=host", "--uts", "--uts=host",
	"--net", "--net=host", "--network=host", "--userns", "--userns=host",
	"--cgroupns", "--cgroupns=host", "--cgroup-parent",
	"--use-api-socket", "--oom-kill-disable", "--restart",
	"-p", "-P", "--publish", "--publish-all",
	"-v", "--volume", // bind mounts go through --mount only
	"-t", "--tty", // never a TTY: -i without -t is the TIOCSTI defence
	"--runtime=nvidia", "--runtime=runsc", // runsc only through W3-2, by the backend
}

// forbiddenRunSubstrings must appear in NO argv element: an unconfined
// security profile, a disabled label, the engine socket, a host or shared
// namespace, a port publish. The golden test scans every element for them, so
// a value smuggled inside another flag's argument is caught too.
var forbiddenRunSubstrings = []string{
	"unconfined",                                    // seccomp=unconfined, apparmor=unconfined, systempaths=unconfined
	"label=disable",                                 // SELinux off
	"docker.sock", "/var/run/docker", "/run/docker", // the engine socket, under any name
	"container:",                             // a shared namespace (W2-9 names the one exception, by the backend)
	"host.docker.internal",                   // the host gateway by name (W2-9 pins declared destinations only)
	"--add-host", "--dns", "--security-opt=", // hyphenated forms of flags the schema sets positionally
}
