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
	// Strip the -H <ep> the backend always passes first.
	if len(args) >= 2 && args[0] == "-H" {
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
	if got := strings.Join(sub, ","); got != "version,info,image" {
		t.Errorf("Available ran %q, want version,info,image", got)
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
	versions := 0
	for _, c := range readCalls(t, logPath) {
		if c.argv[2] == "version" {
			versions++
		}
	}
	if versions != 1 {
		t.Fatalf("docker version was run %d times across two Available calls, want 1 (cached)", versions)
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
