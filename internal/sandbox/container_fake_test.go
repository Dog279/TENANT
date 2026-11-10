package sandbox

// container_fake_test.go drives the container backend against a FAKE docker
// CLI: the test binary itself, re-executed as a helper process (the standard
// os/exec testing trick). The fake records every argv and environment it was
// called with, so the launch rules of W1-1 and the lifecycle rules of W1-4 are
// asserted without an engine:
//
//   - the CLI is called with `-H <vetted endpoint>` first, and never sees a
//     DOCKER_* or NVIDIA_* variable Tenant inherited;
//   - a version below the floors, a missing seccomp or a plaintext daemon on
//     2375 fails Available closed, with the reason;
//   - the probe is cached: a second Available asks the engine nothing;
//   - the startup sweep removes exactly the containers under this install's
//     label.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeDockerEnv = "TENANT_FAKE_DOCKER"

// fakeDockerMain is the helper process: a docker CLI that answers from its
// environment and logs each call. It is entered from TestMain.
func fakeDockerMain() int {
	args := os.Args[1:]
	if logPath := os.Getenv("TENANT_FAKE_DOCKER_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			var env []string
			for _, kv := range os.Environ() {
				up := strings.ToUpper(kv)
				if strings.HasPrefix(up, "DOCKER_") || strings.HasPrefix(up, "NVIDIA_") {
					env = append(env, kv)
				}
			}
			fmt.Fprintf(f, "%s\x1f%s\n", strings.Join(args, "\x1e"), strings.Join(env, "\x1e"))
			f.Close()
		}
	}
	// Strip the -H <ep> the backend always passes first, and the TLS flags
	// that follow it for a tcp:// endpoint.
	if len(args) >= 2 && args[0] == "-H" {
		args = args[2:]
	}
	if len(args) >= 1 && args[0] == "--tlsverify" {
		args = args[1:]
	}
	for len(args) >= 2 && (args[0] == "--tlscacert" || args[0] == "--tlscert" || args[0] == "--tlskey") {
		args = args[2:]
	}
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "version":
		engine := firstNonEmptyStr(os.Getenv("TENANT_FAKE_DOCKER_ENGINE"), "29.8.0")
		fmt.Printf(`{"Client":{"Version":"29.8.0"},"Server":{"Version":%q,"Platform":{"Name":"Docker Desktop 4.92.0"},"Components":[{"Name":"runc","Version":"1.5.1"},{"Name":"containerd","Version":"2.1.5"}]}}`, engine)
		return 0
	case "info":
		if os.Getenv("TENANT_FAKE_DOCKER_NOSECCOMP") == "1" {
			fmt.Print(`{"SecurityOptions":["name=cgroupns"],"OSType":"linux","Driver":"overlay2"}`)
		} else {
			fmt.Print(probeBoxInfo)
		}
		return 0
	case "image":
		// `image inspect --format {{.Id}}` answers the id the tag points at.
		for _, a := range args {
			if a == "{{.Id}}" {
				fmt.Println(firstNonEmptyStr(os.Getenv("TENANT_FAKE_DOCKER_IMAGE_ID"), "sha256:1111"))
			}
		}
		return 0
	case "ps":
		// TENANT_FAKE_DOCKER_PS is "id=instance,id=instance,id": one line per
		// container, the id and its instance label (empty without one), as the
		// sweep's --format asks. TENANT_FAKE_DOCKER_STALL=ps never answers.
		if os.Getenv("TENANT_FAKE_DOCKER_STALL") == "ps" {
			time.Sleep(time.Minute)
			return 0
		}
		for _, entry := range strings.Split(os.Getenv("TENANT_FAKE_DOCKER_PS"), ",") {
			if id, instance, _ := strings.Cut(entry, "="); id != "" {
				fmt.Printf("%s\t%s\n", id, instance)
			}
		}
		return 0
	case "rm", "kill":
		return 0
	case "context":
		return 1
	case "run":
		// TENANT_FAKE_DOCKER_STALL=run never answers, like an engine that
		// cannot start a container. The container check's command gets a uid
		// map (TENANT_FAKE_DOCKER_UIDMAP, default: no user namespace) and a
		// kernel (TENANT_FAKE_DOCKER_KERNEL).
		if os.Getenv("TENANT_FAKE_DOCKER_STALL") == "run" {
			time.Sleep(time.Minute)
			return 0
		}
		for _, a := range args {
			if strings.Contains(a, "/proc/self/uid_map") {
				fmt.Println(firstNonEmptyStr(os.Getenv("TENANT_FAKE_DOCKER_UIDMAP"), "         0          0 4294967295"))
				fmt.Println(firstNonEmptyStr(os.Getenv("TENANT_FAKE_DOCKER_KERNEL"), "6.18.54.1-microsoft-standard-WSL2"))
				return 0
			}
		}
		fmt.Println("fake-run")
		return 0
	}
	return 2
}

// fakeDocker returns a backend whose CLI is the fake, with an endpoint of its
// own (so the shared probe cache is this test's alone), and the path of the
// call log. It pins the OS probes and the 2375 check to neutral answers.
func fakeDocker(t *testing.T) (*containerBackend, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(fakeDockerEnv, "1")
	t.Setenv("TENANT_FAKE_DOCKER_LOG", logPath)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldWSL, oldDesktop, oldPlain := probeWSL, desktopVersionProbe, plaintextDaemonAnswers
	probeWSL = func(context.Context) (string, string) { return "", "" }
	desktopVersionProbe = func() string { return "" }
	plaintextDaemonAnswers = func(context.Context) bool { return false }
	t.Cleanup(func() { probeWSL, desktopVersionProbe, plaintextDaemonAnswers = oldWSL, oldDesktop, oldPlain })
	b := Container(ContainerOptions{CLI: exe, Endpoint: "npipe:////./pipe/" + t.Name(), Instance: "inst-test"})
	return b, logPath
}

type fakeCall struct {
	argv []string
	env  []string // the DOCKER_*/NVIDIA_* variables the fake saw
}

func readCalls(t *testing.T, logPath string) []fakeCall {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		a, e, _ := strings.Cut(line, "\x1f")
		c := fakeCall{argv: strings.Split(a, "\x1e")}
		if e != "" {
			c.env = strings.Split(e, "\x1e")
		}
		calls = append(calls, c)
	}
	return calls
}

// W1-1: every CLI call leads with -H <endpoint>, and inherits no DOCKER_* or
// NVIDIA_* — even when Tenant's own environment points DOCKER_HOST elsewhere.
func TestFakeDocker_CleanLaunch(t *testing.T) {
	b, logPath := fakeDocker(t)
	t.Setenv("DOCKER_HOST", "tcp://evil.example:2375")
	t.Setenv("DOCKER_CONTEXT", "evil")
	t.Setenv("NVIDIA_VISIBLE_DEVICES", "all")
	if err := b.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	calls := readCalls(t, logPath)
	if len(calls) == 0 {
		t.Fatal("the fake CLI was never called")
	}
	for _, c := range calls {
		if len(c.argv) < 2 || c.argv[0] != "-H" || c.argv[1] != "npipe:////./pipe/"+t.Name() {
			t.Errorf("call did not lead with the vetted endpoint: %v", c.argv)
		}
		if len(c.env) != 0 {
			t.Errorf("call %v inherited %v", c.argv[2:], c.env)
		}
	}
	var sub []string
	for _, c := range calls {
		sub = append(sub, c.argv[2])
	}
	// version and info are the probe, image is the image check, run is the
	// container check and kill is its reap.
	if got := strings.Join(sub, ","); got != "version,info,image,run,kill" {
		t.Errorf("Available ran %q, want version,info,image,run,kill", got)
	}
}

// W1-1: the probe is cached on success; a second Available asks nothing.
func TestFakeDocker_ProbeCached(t *testing.T) {
	b, logPath := fakeDocker(t)
	ctx := context.Background()
	if err := b.Available(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Available(ctx); err != nil {
		t.Fatal(err)
	}
	versions, runs := 0, 0
	for _, c := range readCalls(t, logPath) {
		switch c.argv[2] {
		case "version":
			versions++
		case "run":
			runs++
		}
	}
	if versions != 1 {
		t.Fatalf("docker version was run %d times across two Available calls, want 1 (cached)", versions)
	}
	if runs != 1 {
		t.Fatalf("the container check ran %d times across two Available calls, want 1 (cached)", runs)
	}
}

// W1-1: before first use, one container runs through the real argv and
// reports what only a container can: the uid map (a user namespace or not)
// and the kernel. Probe shows it afterwards and never runs it itself.
func TestFakeDocker_ContainerCheck(t *testing.T) {
	b, logPath := fakeDocker(t)
	ctx := context.Background()
	if d := b.Probe(ctx).Detail; !strings.Contains(d, "Container check: not run yet") {
		t.Fatalf("Probe before first use should say the check has not run: %s", d)
	}
	for _, c := range readCalls(t, logPath) {
		if c.argv[2] == "run" {
			t.Fatal("Probe ran a container")
		}
	}
	if err := b.Available(ctx); err != nil {
		t.Fatalf("Available: %v", err)
	}
	if d := b.Probe(ctx).Detail; !strings.Contains(d, "container check: userns off (no ECI), kernel 6.18.54.1-microsoft-standard-WSL2") {
		t.Fatalf("Probe after the check should report it: %s", d)
	}
	// The check container is built like any run's: no network, read-only, and
	// only the scratch work dir mounted.
	var run []string
	for _, c := range readCalls(t, logPath) {
		if c.argv[2] == "run" {
			run = c.argv[2:]
		}
	}
	joined := strings.Join(run, " ")
	for _, want := range []string{"--network none", "--read-only", "--cap-drop ALL", "--pull never"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the check container's argv lacks %q: %s", want, joined)
		}
	}
}

// A user namespace shows in the uid map: anything but the identity map.
func TestFakeDocker_ContainerCheckSeesUserNamespace(t *testing.T) {
	b, _ := fakeDocker(t)
	t.Setenv("TENANT_FAKE_DOCKER_UIDMAP", "         0     100000      65536")
	if err := b.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	if d := b.Probe(context.Background()).Detail; !strings.Contains(d, "container check: userns on") {
		t.Fatalf("a remapped uid 0 should read as a user namespace: %s", d)
	}
}

// An engine that answers `version` and `info` but cannot start a container (a
// Docker Desktop stuck starting) fails Available at the check's deadline, with
// a reason, and is not asked again until the back-off has passed. Without the
// check every command waited out its own wall clock and returned nothing.
func TestFakeDocker_EngineThatCannotStartAContainer(t *testing.T) {
	b, logPath := fakeDocker(t)
	t.Setenv("TENANT_FAKE_DOCKER_STALL", "run")
	old := containerCheckTimeout
	containerCheckTimeout = 400 * time.Millisecond
	t.Cleanup(func() { containerCheckTimeout = old })
	ctx := context.Background()
	start := time.Now()
	err := b.Available(ctx)
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("Available took %s against an engine that cannot start a container", took)
	}
	if err == nil || !strings.Contains(err.Error(), "did not start and finish a container") {
		t.Fatalf("Available = %v, want the reason the engine is unusable", err)
	}
	again := b.Available(ctx)
	if again == nil || again.Error() != err.Error() {
		t.Fatalf("second Available = %v, want the cached refusal", again)
	}
	runs := 0
	for _, c := range readCalls(t, logPath) {
		if c.argv[2] == "run" {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("the check ran %d times inside its back-off, want 1", runs)
	}
}

func TestParseContainerCheck(t *testing.T) {
	cases := []struct {
		name, out  string
		userns     bool
		kernel     string
		wantErrHas string
	}{
		{"no namespace", "         0          0 4294967295\n6.18.54.1-microsoft-standard-WSL2\n", false, "6.18.54.1-microsoft-standard-WSL2", ""},
		{"remapped", "0 100000 65536\n6.19.2\n", true, "6.19.2", ""},
		{"two rows", "0 1000 1\n1 100000 65535\n6.19.2\n", true, "6.19.2", ""},
		{"windows line ends", "0 0 4294967295\r\n6.19.2\r\n", false, "6.19.2", ""},
		{"kernel only", "6.19.2\n", false, "", "not a uid map and a kernel"},
		{"empty", "", false, "", "not a uid map and a kernel"},
		{"not a map", "permission denied\n6.19.2\n", false, "", "not three numbers"},
	}
	for _, c := range cases {
		got, err := parseContainerCheck(c.out)
		if c.wantErrHas != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErrHas) {
				t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.wantErrHas)
			}
			continue
		}
		if err != nil || got.userns != c.userns || got.kernel != c.kernel {
			t.Errorf("%s: got %+v, %v; want userns=%v kernel=%q", c.name, got, err, c.userns, c.kernel)
		}
	}
}

// The floor is about the kernel containers run on. `wsl --version` prints the
// installed kernel; a VM started before an update keeps the old one.
func TestCheckRunningKernel(t *testing.T) {
	cases := []struct {
		name, installed, running string
		refuse, warn             string
	}{
		{"both current", "6.18.58.1-1", "6.18.58.1-microsoft-standard-WSL2", "", ""},
		{"running trails installed, above the floor", "6.18.58.1-1", "6.18.54.1-microsoft-standard-WSL2", "", "still running 6.18.54.1-microsoft-standard-WSL2"},
		{"running below the floor, installed above it", "6.18.58.1-1", "6.18.30.2-microsoft-standard-WSL2", "predates the CVE-2026-53362 fix", ""},
		{"no kernel reported", "6.18.58.1-1", "", "", "reported no kernel version"},
		{"unparseable", "6.18.58.1-1", "custom", "", "could not be parsed"},
		{"no installed line", "", "6.18.54.1-microsoft-standard-WSL2", "", ""},
	}
	for _, c := range cases {
		g := checkRunningKernel(c.installed, c.running)
		refuse, warn := strings.Join(g.refuse, "; "), strings.Join(g.warn, "; ")
		if (c.refuse == "") != (refuse == "") || !strings.Contains(refuse, c.refuse) {
			t.Errorf("%s: refuse = %q, want one containing %q", c.name, refuse, c.refuse)
		}
		if (c.warn == "") != (warn == "") || !strings.Contains(warn, c.warn) {
			t.Errorf("%s: warn = %q, want one containing %q", c.name, warn, c.warn)
		}
	}
}

// W1-1: the floors fail Available closed, with the reason doctor shows.
func TestFakeDocker_RefusesBelowFloor(t *testing.T) {
	b, _ := fakeDocker(t)
	t.Setenv("TENANT_FAKE_DOCKER_ENGINE", "28.3.0")
	err := b.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "engine 28.3.0 is below the floor") {
		t.Fatalf("expected the engine floor refusal, got %v", err)
	}
	if caps := b.Probe(context.Background()); caps.Filesystem == EnforceFull || !strings.Contains(caps.Detail, "floor") {
		t.Fatalf("Probe must not claim enforcement below the floor: %+v", caps)
	}
}

func TestFakeDocker_RefusesWithoutSeccomp(t *testing.T) {
	b, _ := fakeDocker(t)
	t.Setenv("TENANT_FAKE_DOCKER_NOSECCOMP", "1")
	err := b.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "seccomp") {
		t.Fatalf("expected the seccomp refusal, got %v", err)
	}
}

func TestFakeDocker_RefusesPlaintextDaemon(t *testing.T) {
	b, logPath := fakeDocker(t)
	plaintextDaemonAnswers = func(context.Context) bool { return true }
	err := b.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), plaintextDaemonAddr) {
		t.Fatalf("expected the 2375 refusal, got %v", err)
	}
	if calls := readCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("the CLI must not be called while 2375 answers, got %d calls", len(calls))
	}
}

func TestFakeDocker_RefusesBadEndpoint(t *testing.T) {
	b, _ := fakeDocker(t)
	b.explicitEP = "tcp://10.0.0.9:2375"
	err := b.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without TLS") {
		t.Fatalf("expected the tcp refusal, got %v", err)
	}
}

// W1-6: the container's digest of a granted run is stable for the same run,
// and moves when the image the tag points at, the granted folder, the command
// or the network grant does. A scratch work dir (a new temp folder each time)
// does not move it.
func TestFakeDocker_GrantedRunHash(t *testing.T) {
	b, _ := fakeDocker(t)
	ctx := context.Background()
	dirA, dirB := t.TempDir(), t.TempDir()
	refusedAt := func(dir string) Result {
		return Result{ExitCode: 1, Violations: []Violation{{Kind: "filesystem", Detail: "read-only", Path: dir}}}
	}
	hash := func(cmd string, pol Policy, refused Result) string {
		t.Helper()
		h, err := b.GrantedRunHash(ctx, Command{Shell: cmd}, pol, refused)
		if err != nil || h == "" {
			t.Fatalf("GrantedRunHash: %q, %v", h, err)
		}
		return h
	}
	pol := Policy{Network: NetNone, Enforce: true}
	base := hash("touch x", pol, refusedAt(dirA))
	if again := hash("touch x", pol, refusedAt(dirA)); again != base {
		t.Fatal("the same run hashed differently twice (a scratch work dir must not count)")
	}
	if hash("touch y", pol, refusedAt(dirA)) == base {
		t.Error("another command has the same digest")
	}
	if hash("touch x", pol, refusedAt(dirB)) == base {
		t.Error("another granted folder has the same digest")
	}
	if hash("touch x", pol, Result{ExitCode: 1, Violations: []Violation{{Kind: "network", Detail: "no route"}}}) == base {
		t.Error("a network grant has the same digest as a folder grant")
	}
	t.Setenv("TENANT_FAKE_DOCKER_IMAGE_ID", "sha256:2222")
	if hash("touch x", pol, refusedAt(dirA)) == base {
		t.Error("the image tag now points at another image and the digest did not move")
	}
	if _, err := b.GrantedRunHash(ctx, Command{Shell: "touch x"}, pol, Result{ExitCode: 1, Violations: []Violation{{Kind: "limit", Detail: "oom"}}}); err == nil {
		t.Error("a refusal with nothing to grant must have no digest")
	}
}

// A tcp:// endpoint is used only with mutual TLS, and the CLI is told so on
// its command line: the environment that would switch TLS on is stripped, so
// `-H tcp://…` alone would be a plain-HTTP connection to an engine that is
// root on its host. Without all three files the endpoint is refused.
func TestFakeDocker_TCPEndpointIsAlwaysLaunchedWithTLS(t *testing.T) {
	_, logPath := fakeDocker(t) // pins the probes and sets the fake's environment
	exe, _ := os.Executable()
	certs := t.TempDir()
	ctx := context.Background()

	bare := Container(ContainerOptions{CLI: exe, Endpoint: "tcp://10.0.0.9:2376", TLSCertDir: certs, Instance: "inst-test"})
	err := bare.Available(ctx)
	if err == nil || !strings.Contains(err.Error(), "needs mutual TLS") || !strings.Contains(err.Error(), "ca.pem") {
		t.Fatalf("a tcp:// endpoint with no certificate files must be refused, got %v", err)
	}
	if calls := readCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("the CLI was called for a tcp:// endpoint that has no TLS material: %v", calls[0].argv)
	}

	for _, f := range []string{"ca.pem", "cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(certs, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := Container(ContainerOptions{CLI: exe, Endpoint: "tcp://10.0.0.10:2376", TLSCertDir: certs, Instance: "inst-test"})
	if err := b.Available(ctx); err != nil {
		t.Fatalf("Available with TLS material: %v", err)
	}
	calls := readCalls(t, logPath)
	if len(calls) == 0 {
		t.Fatal("the fake CLI was never called")
	}
	want := []string{"-H", "tcp://10.0.0.10:2376", "--tlsverify",
		"--tlscacert", filepath.Join(certs, "ca.pem"), "--tlscert", filepath.Join(certs, "cert.pem"), "--tlskey", filepath.Join(certs, "key.pem")}
	for _, c := range calls {
		if len(c.argv) <= len(want) || strings.Join(c.argv[:len(want)], "|") != strings.Join(want, "|") {
			t.Errorf("a call to a tcp:// endpoint did not lead with the endpoint and its TLS flags: %v", c.argv)
		}
	}
}

// A local or ssh endpoint gets no TLS flags.
func TestTLSArgs_OnlyForTCP(t *testing.T) {
	b := Container(ContainerOptions{TLSCertDir: t.TempDir()})
	for _, ep := range []string{"npipe:////./pipe/dockerDesktopLinuxEngine", "unix:///var/run/docker.sock", "ssh://ops@build-host"} {
		if args, err := b.tlsArgs(ep); err != nil || len(args) != 0 {
			t.Errorf("tlsArgs(%q) = %v, %v; want nothing", ep, args, err)
		}
	}
}

// W1-4: the startup sweep lists the containers with their instance label and
// removes exactly this install's: not another install's, not an unlabelled
// one. The engine's own label filter is not used (it stalled on one Docker
// Desktop; see controlTimeout).
func TestFakeDocker_SweepLeftovers(t *testing.T) {
	b, logPath := fakeDocker(t)
	t.Setenv("TENANT_FAKE_DOCKER_PS", "aaa111=inst-test,zzz999=inst-other,bbb222=inst-test,ccc333,ddd444=inst-test-2")
	n, err := b.SweepLeftovers(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("SweepLeftovers = (%d, %v), want (2, nil)", n, err)
	}
	var seen []string
	for _, c := range readCalls(t, logPath) {
		seen = append(seen, strings.Join(c.argv[2:], " "))
	}
	want := []string{
		"ps -a --no-trunc --format {{.ID}}\t{{.Label \"" + labelInstance + "\"}}",
		"rm -f aaa111",
		"rm -f bbb222",
	}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("sweep ran %q, want %q", seen, want)
	}
}

// W2-7: stop-all removes every container labelled as a Tenant sandbox,
// whichever install started it, and nothing else on the engine.
func TestStopAllContainers(t *testing.T) {
	b, logPath := fakeDocker(t)
	SetContainerDefaults(ContainerOptions{CLI: b.cli, Endpoint: b.explicitEP, Instance: b.instance})
	t.Cleanup(func() { SetContainerDefaults(ContainerOptions{}) })
	// The fake prints "id<TAB>value" for whichever label is asked for; "1" is
	// the sandbox label's value, anything else is another container.
	t.Setenv("TENANT_FAKE_DOCKER_PS", "aaa111=1,redis999,bbb222=1,ccc333=0")
	found, failed, err := StopAllContainers(context.Background())
	if err != nil || found != 2 || failed != 0 {
		t.Fatalf("StopAllContainers = (%d, %d, %v), want (2, 0, nil)", found, failed, err)
	}
	var seen []string
	for _, c := range readCalls(t, logPath) {
		seen = append(seen, strings.Join(c.argv[2:], " "))
	}
	want := []string{
		"ps -a --no-trunc --format {{.ID}}\t{{.Label \"" + labelSandbox + "\"}}",
		"rm -f aaa111",
		"rm -f bbb222",
	}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("stop-all ran %q, want %q", seen, want)
	}

	t.Setenv("TENANT_FAKE_DOCKER_PS", "")
	if found, failed, err := StopAllContainers(context.Background()); err != nil || found != 0 || failed != 0 {
		t.Fatalf("nothing to stop = (%d, %d, %v)", found, failed, err)
	}
}

// A docker CLI that never answers must not hold start-up: the sweep gives up
// at its deadline with an error that says the CLI stalled, and removes
// nothing. (serve waited on this call with no deadline.)
func TestSweepContainers_StalledCLIIsBounded(t *testing.T) {
	b, logPath := fakeDocker(t)
	t.Setenv("TENANT_FAKE_DOCKER_STALL", "ps")
	oldControl, oldSweep := controlTimeout, sweepTimeout
	controlTimeout, sweepTimeout = 300*time.Millisecond, 2*time.Second
	SetContainerDefaults(ContainerOptions{CLI: b.cli, Endpoint: b.explicitEP, Instance: b.instance})
	t.Cleanup(func() {
		controlTimeout, sweepTimeout = oldControl, oldSweep
		SetContainerDefaults(ContainerOptions{})
	})
	start := time.Now()
	n, err := SweepContainers(context.Background())
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the sweep took %s with a stalled CLI; it must give up at its deadline", took)
	}
	if n != 0 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("SweepContainers = (%d, %v), want (0, an error saying the CLI did not answer)", n, err)
	}
	for _, c := range readCalls(t, logPath) {
		if len(c.argv) > 2 && c.argv[2] == "rm" {
			t.Fatalf("the sweep removed a container after the listing stalled: %q", c.argv)
		}
	}
}

// SweepContainers (the startup hook) uses the process defaults and is a no-op,
// not an error, when the CLI is absent.
func TestSweepContainers_NoCLIIsNotAnError(t *testing.T) {
	SetContainerDefaults(ContainerOptions{CLI: "definitely-not-a-docker-binary-" + t.Name()})
	t.Cleanup(func() { SetContainerDefaults(ContainerOptions{}) })
	if n, err := SweepContainers(context.Background()); n != 0 || err != nil {
		t.Fatalf("SweepContainers = (%d, %v), want (0, nil)", n, err)
	}
}

// For("container") builds its backend from the process defaults: the
// install's instance id, so the containers a surface starts carry the label
// the start-up sweep looks for. (It built with empty options, so every command
// was labelled with a per-process id and the sweep could never find a previous
// run's leftovers.)
func TestFor_ContainerUsesTheProcessDefaults(t *testing.T) {
	SetContainerDefaults(ContainerOptions{Instance: "inst-from-defaults", Endpoint: "npipe:////./pipe/" + t.Name()})
	t.Cleanup(func() { SetContainerDefaults(ContainerOptions{}) })
	be, err := For("container")
	if err != nil {
		t.Fatal(err)
	}
	b, ok := be.(*containerBackend)
	if !ok {
		t.Fatalf("For(container) = %T", be)
	}
	if b.instance != "inst-from-defaults" || b.explicitEP != "npipe:////./pipe/"+t.Name() {
		t.Fatalf("For(container) built instance %q endpoint %q, want the process defaults", b.instance, b.explicitEP)
	}
	if sweep := Container(containerDefaults()); sweep.instance != b.instance {
		t.Fatalf("the sweep looks for %q but commands are labelled %q", sweep.instance, b.instance)
	}
}

// Every reason Available refuses for carries the fix doctor shows.
func TestFakeDocker_RefusalsCarryAFix(t *testing.T) {
	fix := func(err error) string {
		var u *Unavailable
		if errors.As(err, &u) {
			return u.Fix
		}
		return ""
	}
	ctx := context.Background()

	missing := Container(ContainerOptions{CLI: "definitely-not-a-docker-binary-" + t.Name()})
	if err := missing.Available(ctx); fix(err) != fixInstallEngine {
		t.Errorf("no CLI: %v, fix %q", err, fix(err))
	}

	b, _ := fakeDocker(t)
	t.Setenv("TENANT_FAKE_DOCKER_ENGINE", "20.10.0")
	if err := b.Available(ctx); fix(err) != fixFloors {
		t.Errorf("below the floor: %v, fix %q", err, fix(err))
	}

	b2, _ := fakeDocker(t)
	b2.explicitEP = "npipe:////./pipe/" + t.Name() + "-plain"
	b2.cache = sharedProbeCache(b2.cli + "\x00" + b2.explicitEP)
	t.Setenv("TENANT_FAKE_DOCKER_ENGINE", "")
	plaintextDaemonAnswers = func(context.Context) bool { return true }
	if err := b2.Available(ctx); fix(err) != fixPlaintext {
		t.Errorf("plaintext daemon: %v, fix %q", err, fix(err))
	}
	plaintextDaemonAnswers = func(context.Context) bool { return false }

	b3, _ := fakeDocker(t)
	b3.explicitEP = "npipe:////./pipe/" + t.Name() + "-stuck"
	b3.cache = sharedProbeCache(b3.cli + "\x00" + b3.explicitEP)
	t.Setenv("TENANT_FAKE_DOCKER_STALL", "run")
	old := containerCheckTimeout
	containerCheckTimeout = 400 * time.Millisecond
	t.Cleanup(func() { containerCheckTimeout = old })
	if err := b3.Available(ctx); fix(err) != fixStuckEngine {
		t.Errorf("cannot start a container: %v, fix %q", err, fix(err))
	}
}

func TestInstanceID(t *testing.T) {
	a := InstanceID(filepath.Join("C:\\", "Users", "u", "AppData", "tenant"))
	b := InstanceID(filepath.Join("C:\\", "Users", "u", "AppData", "tenant"))
	c := InstanceID(filepath.Join("C:\\", "Users", "v", "AppData", "tenant"))
	if a != b || a == c || !strings.HasPrefix(a, "inst-") {
		t.Fatalf("InstanceID not stable per data dir: %q %q %q", a, b, c)
	}
	if _, err := json.Marshal(a); err != nil {
		t.Fatal(err)
	}
}
