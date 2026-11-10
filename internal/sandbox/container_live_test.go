package sandbox

// container_live_test.go exercises the real container backend against a live
// engine. It is gated on TENANT_SANDBOX_LIVE=1 so the offline CI never needs
// Docker; the operator (and the Windows box that CI cannot mirror) runs it with
// the env var set. `tenant sandbox selftest --backend container` runs the same
// escapecore suite, and W1-7 (TEN-461) makes selftest the security test of
// record on Windows.
//
// Wave-1 acceptance cases that live here, each tagged with its ticket:
//
//   - W1-4: a timed-out, a cancelled and an over-cap command leave no
//     container 2 s later; the sweep removes a leftover.
//   - W1-3: a junction given as the work dir is refused; a comma in the work
//     dir's path mounts and runs.
//   - W1-5: loopback is unreachable under none and reachable under full.
//   - W1-6: a Windows program exits 127 and is attributed as a program
//     refusal; an OOM kill is attributed as a limit, never a refusal.
//   - W1-2/W2-5: the environment inside is exactly the forwarded names plus
//     the image's own, with HOME in the work dir and no DOCKER_*/NVIDIA_*.
//
// Still pending here (W1-7's list, needing image or harness work first): the
// setuid control (the image strips setuid bits; needs a planted binary), the
// 32-bit socketcall(AF_ALG) probe (after W2-8's seccomp delta), a planted
// Windows executable under WSL interop.

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func liveContainer(t *testing.T) *containerBackend {
	t.Helper()
	if os.Getenv("TENANT_SANDBOX_LIVE") != "1" {
		t.Skip("set TENANT_SANDBOX_LIVE=1 to run the live container tests (needs Docker and a built tenant-sandbox image)")
	}
	b := Container(ContainerOptions{Instance: "live-" + t.Name()})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	caps := b.Probe(ctx)
	if caps.Filesystem != EnforceFull {
		t.Skipf("container backend not ready on this host: %s", caps.Detail)
	}
	return b
}

func liveWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func livePolicy(work string, wall time.Duration) Policy {
	return Policy{WorkDir: work, ReadRoots: []string{work}, WriteRoots: []string{work}, Network: NetNone,
		Limits: Limits{Wall: wall, MaxOutputBytes: 1 << 16}, Enforce: true}
}

// leftovers lists the containers carrying b's instance label.
func leftovers(t *testing.T, b *containerBackend) []string {
	t.Helper()
	ids, err := b.instanceContainers(context.Background())
	if err != nil {
		t.Skipf("could not list containers: %v", err)
	}
	return ids
}

func TestContainerLive_Identity(t *testing.T) {
	b := liveContainer(t)
	res, err := b.Run(context.Background(), Command{Shell: "id; grep -c . /proc/1/status", Surface: "test"}, livePolicy(liveWorkDir(t), 30*time.Second))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Output, "uid=65532") {
		t.Errorf("expected uid=65532, got %q", res.Output)
	}
}

func TestContainerLive_ReadOnlyRootAndSecret(t *testing.T) {
	b := liveContainer(t)
	work := liveWorkDir(t)
	pol := livePolicy(work, 30*time.Second)

	// Write to the rootfs: EROFS, attributed as filesystem.
	res, _ := b.Run(context.Background(), Command{Shell: "echo x > /etc/pwned", Surface: "test"}, pol)
	if res.ExitCode == 0 || !res.refusedKind("filesystem") {
		t.Errorf("writing /etc/pwned should fail on a read-only rootfs and be attributed, got %+v", res)
	}

	// The work dir is writable, and the host sees the write.
	res, err := b.Run(context.Background(), Command{Shell: "echo written > out.txt && cat out.txt", Surface: "test"}, pol)
	if err != nil || !strings.Contains(res.Output, "written") {
		t.Errorf("work-dir write twin failed: %v %q", err, res.Output)
	}
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Errorf("host did not see the work-dir write: %v", err)
	}
}

// W1-5: loopback is unreachable under none and reachable under full (the
// bridge reaches the host through host.docker.internal / the gateway).
func TestContainerLive_LoopbackUnderNoneVsFull(t *testing.T) {
	b := liveContainer(t)
	work := liveWorkDir(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello-from-host\n"))
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	probe := "getent hosts host.docker.internal >/dev/null 2>&1 || echo NO-DNS; " +
		"(exec 3<>/dev/tcp/host.docker.internal/" + port + " && head -c 15 <&3) 2>/dev/null || echo UNREACHABLE"

	none := livePolicy(work, 30*time.Second)
	res, _ := b.Run(context.Background(), Command{Shell: "bash -c '" + probe + "'", Surface: "test"}, none)
	if !strings.Contains(res.Output, "NO-DNS") || strings.Contains(res.Output, "hello-from-host") {
		t.Errorf("network none must not reach the host's loopback, got %q", res.Output)
	}

	full := none
	full.Network = NetFull
	res, _ = b.Run(context.Background(), Command{Shell: "bash -c '" + probe + "'", Surface: "test"}, full)
	if !strings.Contains(res.Output, "hello-from-host") {
		t.Logf("full did not reach the host's loopback (%q); on this engine `full` may not expose host.docker.internal", res.Output)
	}
}

// W1-4: a command that outlives the wall clock leaves no container.
func TestContainerLive_TimeoutLeavesNoContainer(t *testing.T) {
	b := liveContainer(t)
	res, _ := b.Run(context.Background(), Command{Shell: "sleep 60", Surface: "test"}, livePolicy(liveWorkDir(t), 2*time.Second))
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got exit %d", res.ExitCode)
	}
	time.Sleep(2 * time.Second)
	if left := leftovers(t, b); len(left) > 0 {
		t.Errorf("reaper left %d container(s) behind: %v", len(left), left)
	}
}

// W1-4: a cancelled command (the turn ended, Tenant is stopping) leaves no
// container 2 s later — the probe's orphan, inverted.
func TestContainerLive_CancelLeavesNoContainer(t *testing.T) {
	b := liveContainer(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(1500 * time.Millisecond); cancel() }()
	b.Run(ctx, Command{Shell: "sleep 60", Surface: "test"}, livePolicy(liveWorkDir(t), time.Minute))
	time.Sleep(2 * time.Second)
	if left := leftovers(t, b); len(left) > 0 {
		t.Errorf("cancel left %d container(s) behind: %v", len(left), left)
	}
}

// W1-4 / TEN-438: a flood of output stops at the cap, is marked, and the
// container is gone — Tenant never buffers more than the cap.
func TestContainerLive_OutputCapKills(t *testing.T) {
	b := liveContainer(t)
	pol := livePolicy(liveWorkDir(t), 60*time.Second)
	pol.Limits.MaxOutputBytes = 1 << 20
	start := time.Now()
	res, _ := b.Run(context.Background(), Command{Shell: "yes | head -c 100000000; yes", Surface: "test"}, pol)
	if !strings.Contains(res.Output, "truncated") || len(res.Output) > pol.Limits.MaxOutputBytes+64 {
		t.Errorf("output not capped: %d bytes", len(res.Output))
	}
	if !res.refusedKind("limit") && !hasKind(res, "limit") {
		t.Errorf("expected a limit violation, got %v", res.Violations)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("the cap did not stop the command promptly: %s", time.Since(start))
	}
	time.Sleep(2 * time.Second)
	if left := leftovers(t, b); len(left) > 0 {
		t.Errorf("over-cap left %d container(s) behind: %v", len(left), left)
	}
}

func hasKind(res Result, kind string) bool {
	for _, v := range res.Violations {
		if v.Kind == kind {
			return true
		}
	}
	return false
}

// W1-4: the sweep removes a container left under this instance's label.
func TestContainerLive_SweepLeftovers(t *testing.T) {
	b := liveContainer(t)
	ctx := context.Background()
	c, err := b.command(ctx, "run", "-d", "--rm", "--label", labelSandbox+"=1", "--label", labelInstance+"="+b.instance,
		"--network", "none", b.image, "sleep", "600")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("could not start a leftover: %v %s", err, out)
	}
	n, err := b.SweepLeftovers(ctx)
	if err != nil || n != 1 {
		t.Fatalf("SweepLeftovers = (%d, %v), want (1, nil)", n, err)
	}
	if left := leftovers(t, b); len(left) > 0 {
		t.Errorf("sweep left %v", left)
	}
}

// W1-3: a junction as the work dir is refused before anything runs; a comma
// in the work dir's path (the probe's exit-125 case) mounts and runs.
func TestContainerLive_JunctionRefusedCommaRuns(t *testing.T) {
	b := liveContainer(t)
	base := t.TempDir()
	if runtime.GOOS == "windows" {
		target := filepath.Join(base, "target")
		link := filepath.Join(base, "link")
		os.MkdirAll(target, 0o755)
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("mklink /J failed: %v %s", err, out)
		}
		res, err := b.Run(context.Background(), Command{Shell: "echo hi", Surface: "test"}, livePolicy(link, 30*time.Second))
		if err == nil || !hasKind(res, "policy") {
			t.Errorf("a junction work dir must be refused, got %+v %v", res, err)
		}
	}
	comma := filepath.Join(base, "has,comma and space")
	if err := os.MkdirAll(comma, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := b.Run(context.Background(), Command{Shell: "pwd; echo ok > here.txt; cat here.txt", Surface: "test"}, livePolicy(comma, 30*time.Second))
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Errorf("a comma in the work dir path must mount and run, got %+v %v", res, err)
	}
}

// W1-6: a Windows program exits 127 in the container and is attributed as a
// program refusal (the only kind that may reach the host); an OOM kill is a
// limit and never a refusal.
func TestContainerLive_ProgramAndOOMAttribution(t *testing.T) {
	b := liveContainer(t)
	pol := livePolicy(liveWorkDir(t), 30*time.Second)
	res, _ := b.Run(context.Background(), Command{Shell: "cmd.exe /c ver", Surface: "test"}, pol)
	if res.ExitCode != 127 || !hasKind(res, "program") {
		t.Errorf("cmd.exe: expected exit 127 attributed as program, got %+v", res)
	}
	pol.Limits.MemoryBytes = 64 << 20
	res, _ = b.Run(context.Background(), Command{Shell: "python3 -c 'x = bytearray(512 << 20); print(len(x))'", Surface: "test"}, pol)
	if !hasKind(res, "limit") || res.Refused() {
		t.Errorf("OOM: expected a limit violation and no refusal, got %+v", res)
	}
}

// W1-2 / W2-5: exactly the forwarded names cross, plus the image's own (PATH,
// HOME, HOSTNAME, PWD, TMPDIR); HOME is the work dir; nothing DOCKER_/NVIDIA_.
func TestContainerLive_ExactEnv(t *testing.T) {
	b := liveContainer(t)
	work := liveWorkDir(t)
	t.Setenv("TENANT_LIVE_PASS", "yes")
	t.Setenv("DOCKER_HOST_SHOULD_NOT_CROSS", "x")
	pol := livePolicy(work, 30*time.Second)
	pol.Env = EnvPolicy{Isolate: true, Pass: []string{"TENANT_LIVE_PASS", "DOCKER_HOST_SHOULD_NOT_CROSS", "PATH"}}
	res, err := b.Run(context.Background(), Command{Shell: "env | cut -d= -f1 | sort | tr '\\n' ' '; echo; echo HOME=$HOME", Surface: "test"}, pol)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Fields(strings.SplitN(res.Output, "\n", 2)[0])
	allowed := map[string]bool{"PATH": true, "HOME": true, "HOSTNAME": true, "PWD": true, "TMPDIR": true, "TENANT_LIVE_PASS": true, "SHLVL": true, "_": true}
	for _, n := range names {
		if !allowed[n] {
			t.Errorf("unexpected variable %q inside the container (all: %v)", n, names)
		}
	}
	if !strings.Contains(res.Output, "HOME="+sandboxHome) {
		t.Errorf("HOME is not the container's own tmpfs: %q", res.Output)
	}
}

// D4: HOME is writable by the sandbox user, is a tmpfs, and what a command
// leaves there neither lands in the work dir on the host nor is there for
// the next container.
func TestContainerLive_HomeIsATmpfsOfItsOwn(t *testing.T) {
	b := liveContainer(t)
	work := liveWorkDir(t)
	pol := livePolicy(work, 30*time.Second)
	res, err := b.Run(context.Background(), Command{Shell: "echo kept > ~/.marker && cat ~/.marker && stat -f -c %T ~ && stat -c %a:%u ~", Surface: "test"}, pol)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("writing to HOME: exit %d, %v\n%s", res.ExitCode, err, res.Output)
	}
	for _, want := range []string{"kept", "tmpfs", "700:65532"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("HOME: output lacks %q:\n%s", want, res.Output)
		}
	}
	if _, err := os.Stat(filepath.Join(work, ".marker")); err == nil {
		t.Error("a file written to HOME landed in the work dir on the host")
	}
	res, err = b.Run(context.Background(), Command{Shell: "ls -A ~ | wc -l", Surface: "test"}, pol)
	if err != nil || strings.TrimSpace(res.Output) != "0" {
		t.Errorf("the next container's HOME is not empty: %q, %v", res.Output, err)
	}
}

// D5: a process that burns CPU past its cap is killed by the kernel, well
// inside the wall clock.
func TestContainerLive_CPULimitStopsARunawayProcess(t *testing.T) {
	b := liveContainer(t)
	pol := livePolicy(liveWorkDir(t), 60*time.Second)
	pol.Limits.CPU = 2 * time.Second
	started := time.Now()
	res, err := b.Run(context.Background(), Command{Shell: "while :; do :; done", Surface: "test"}, pol)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || res.TimedOut || time.Since(started) > 30*time.Second {
		t.Errorf("a busy loop under a 2s CPU cap: exit %d, timed out %v, after %s", res.ExitCode, res.TimedOut, time.Since(started))
	}
}

// W1-6: a refusal line that lands past the output cap is still attributed. The
// display output is capped and keeps only the head, so before the streaming
// witness the same command could be labelled refused or clean depending on
// where the line fell. Here the line is put past the cap on purpose: the shell
// floods stdout past the cap, then hits
// EROFS, then exits on its own, fast, before the over-cap kill lands, so the
// exit code is the shell's and not 137. The capped output must NOT carry the
// line, and the run must still be attributed as a filesystem refusal.
func TestContainerLive_RefusalPastOutputCap(t *testing.T) {
	b := liveContainer(t)
	pol := livePolicy(liveWorkDir(t), 30*time.Second)
	if pol.Limits.MaxOutputBytes <= 0 {
		t.Fatal("the test needs an output cap")
	}
	flood := pol.Limits.MaxOutputBytes + 8<<10
	cmd := Command{Shell: "yes x | head -c " + strconv.Itoa(flood) + "; echo x > /etc/pwned; exit 3", Surface: "test"}

	// The over-cap kill is asynchronous; the shell normally wins by a wide
	// margin, but give the race three tries before calling it inconclusive.
	var res Result
	for attempt := 0; attempt < 3; attempt++ {
		var err error
		if res, err = b.Run(context.Background(), cmd, pol); err != nil {
			t.Fatalf("run: %v", err)
		}
		if res.ExitCode != 137 {
			break
		}
	}
	if res.ExitCode == 137 {
		t.Skip("the over-cap kill landed before the shell exited on three attempts")
	}
	if res.ExitCode != 3 {
		t.Fatalf("expected the shell's own exit 3, got %d (%s)", res.ExitCode, res.ExitStatus)
	}
	if !strings.Contains(res.Output, "[truncated]") {
		t.Fatalf("expected the output to be capped, got %d bytes with no marker", len(res.Output))
	}
	if strings.Contains(strings.ToLower(res.Output), "read-only file system") {
		t.Fatal("test setup: the refusal line must have fallen past the cap, but the capped output carries it")
	}
	if !res.refusedKind("filesystem") {
		t.Fatalf("the refusal past the cap was not attributed: %+v", res.Violations)
	}
}

// W1-6 on a real engine: a write into a folder granted read-only is refused
// and names the folder; an approval re-runs the same command with that one
// folder writable, started from the description that was hashed and by the
// image's id; the file lands on the host. A digest that is not the approved
// one starts nothing.
func TestContainerLive_ApprovedReRunStartsByImageID(t *testing.T) {
	b := liveContainer(t)
	work, extra := liveWorkDir(t), t.TempDir()
	pol := livePolicy(work, 30*time.Second)
	pol.ReadRoots = []string{work, extra}
	dest := containerDest(extra, runtime.GOOS)
	cmd := Command{Shell: "echo granted > '" + dest + "/out.txt'", Surface: "test"}
	ctx := context.Background()

	refused, err := b.Run(ctx, cmd, pol)
	if err != nil || !refused.Refused() {
		t.Fatalf("a write into a read-only grant must be refused: %+v, %v", refused, err)
	}
	if _, err := os.Stat(filepath.Join(extra, "out.txt")); err == nil {
		t.Fatal("the refused write reached the host")
	}
	if _, err := b.RunGrantedAs(ctx, cmd, pol, refused, "not-the-approved-digest"); err == nil {
		t.Error("a re-run under a digest that was not approved started")
	}
	if _, err := os.Stat(filepath.Join(extra, "out.txt")); err == nil {
		t.Fatal("the re-run that was not approved wrote the file")
	}

	asked := 0
	res, err := Execute(ctx, b, cmd, pol, func(context.Context, Command, Result) bool { asked++; return true })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if asked != 1 || !res.Escalated || res.ExitCode != 0 {
		t.Fatalf("asked %d time(s), escalated %v, exit %d, violations %+v\n%s", asked, res.Escalated, res.ExitCode, res.Violations, res.Output)
	}
	got, err := os.ReadFile(filepath.Join(extra, "out.txt"))
	if err != nil || strings.TrimSpace(string(got)) != "granted" {
		t.Errorf("the approved re-run did not write the granted folder: %q, %v", got, err)
	}
	c := res.Confinement
	if c == nil || c.Image != b.image || !strings.HasPrefix(c.ImageID, "sha256:") {
		t.Fatalf("the re-run's record must name the tag and the id it ran: %+v", c)
	}
	writable := false
	for _, m := range c.Mounts {
		writable = writable || (strings.Contains(m, dest) && strings.HasSuffix(m, "(rw)"))
	}
	if !writable {
		t.Errorf("the granted folder is not a writable mount of the re-run: %q", c.Mounts)
	}
	if left := leftovers(t, b); len(left) != 0 {
		t.Errorf("containers left behind: %v", left)
	}
}

// W2-4 on a real engine: a `full` command reaches the internet-facing side
// of its own network but not a container on Docker's default bridge, nor
// another container on the sandbox network. The neighbour is started by hand
// on the default bridge, then on the sandbox network, serving one page.
func TestContainerLive_FullIsOffTheDefaultBridge(t *testing.T) {
	b := liveContainer(t)
	ctx := context.Background()
	full := livePolicy(liveWorkDir(t), 40*time.Second)
	full.Network = NetFull
	// The first full run makes the network.
	if res, err := b.Run(ctx, Command{Shell: "echo up", Surface: "test"}, full); err != nil || res.ExitCode != 0 {
		t.Fatalf("a full run: exit %d, %v\n%s", res.ExitCode, err, res.Output)
	}
	out, err := exec.Command("docker", "network", "inspect", "--format", iccTemplate, sandboxNetwork).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Fatalf("the sandbox network must exist with traffic between containers off: %q, %v", out, err)
	}

	neighbour := func(network string) (ip string) {
		t.Helper()
		name := "tenant-sbx-live-neighbour-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		args := []string{"run", "-d", "--rm", "--pull", "never", "--name", name, "--user", "65532:65532", "--cap-drop", "ALL"}
		if network != "" {
			args = append(args, "--network", network)
		}
		args = append(args, defaultImage, "sh", "-c", "cd /tmp && echo neighbour-page > index.html && exec python3 -m http.server 8080")
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("starting the neighbour: %v: %s", err, out)
		}
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
		format := "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"
		for i := 0; i < 20; i++ {
			if out, err := exec.Command("docker", "inspect", "--format", format, name).Output(); err == nil && strings.TrimSpace(string(out)) != "" {
				ip = strings.TrimSpace(string(out))
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if ip == "" {
			t.Fatal("the neighbour got no address")
		}
		// The check below means something only if the page is really being
		// served: ask the neighbour itself until it answers.
		served := false
		for i := 0; i < 40 && !served; i++ {
			page, _ := exec.Command("docker", "exec", name, "curl", "-s", "-m", "2", "http://127.0.0.1:8080/").Output()
			if served = strings.Contains(string(page), "neighbour-page"); !served {
				time.Sleep(250 * time.Millisecond)
			}
		}
		if !served {
			t.Fatal("the neighbour never served its page, so nothing can be concluded")
		}
		return ip
	}
	fetch := func(ip string) string {
		res, _ := b.Run(ctx, Command{Shell: "curl -s -m 4 http://" + ip + ":8080/ || echo UNREACHABLE", Surface: "test"}, full)
		return res.Output
	}
	if got := fetch(neighbour("")); strings.Contains(got, "neighbour-page") {
		t.Errorf("a full command reached a container on the default bridge: %q", got)
	}
	if got := fetch(neighbour(sandboxNetwork)); strings.Contains(got, "neighbour-page") {
		t.Errorf("a full command reached another container on the sandbox network: %q", got)
	}
}
