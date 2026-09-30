package sandbox

// container_live_test.go exercises the real container backend against a live
// engine. It is gated on TENANT_SANDBOX_LIVE=1 so the offline CI never needs
// Docker; the operator (and the Windows box that CI cannot mirror) runs it with
// the env var set. `tenant sandbox selftest` runs the same escapecore suite.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func liveContainer(t *testing.T) *containerBackend {
	t.Helper()
	if os.Getenv("TENANT_SANDBOX_LIVE") != "1" {
		t.Skip("set TENANT_SANDBOX_LIVE=1 to run the live container tests (needs Docker and a built tenant-sandbox image)")
	}
	b := Container(ContainerOptions{})
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
	if err := os.WriteFile(dir+string(os.PathSeparator)+"in.txt", []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestContainerLive_Identity(t *testing.T) {
	b := liveContainer(t)
	pol := Policy{WorkDir: liveWorkDir(t), Network: NetNone, Limits: Limits{Wall: 30 * time.Second, MaxOutputBytes: 1 << 16}}
	res, err := b.Run(context.Background(), Command{Shell: "id; grep -c . /proc/1/status", Surface: "test"}, pol)
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
	pol := Policy{WorkDir: work, ReadRoots: []string{work}, WriteRoots: []string{work}, Network: NetNone,
		Limits: Limits{Wall: 30 * time.Second, MaxOutputBytes: 1 << 16}}

	// Write to the rootfs: EROFS, attributed as filesystem.
	res, _ := b.Run(context.Background(), Command{Shell: "echo x > /etc/pwned", Surface: "test"}, pol)
	if res.ExitCode == 0 {
		t.Error("writing /etc/pwned should fail on a read-only rootfs")
	}

	// The work dir is writable, and the host sees the write.
	res, err := b.Run(context.Background(), Command{Shell: "echo written > out.txt && cat out.txt", Surface: "test"}, pol)
	if err != nil || !strings.Contains(res.Output, "written") {
		t.Errorf("work-dir write twin failed: %v %q", err, res.Output)
	}
	if _, err := os.Stat(work + string(os.PathSeparator) + "out.txt"); err != nil {
		t.Errorf("host did not see the work-dir write: %v", err)
	}
}

func TestContainerLive_NetworkNoneVsFull(t *testing.T) {
	b := liveContainer(t)
	work := liveWorkDir(t)
	none := Policy{WorkDir: work, Network: NetNone, Limits: Limits{Wall: 30 * time.Second, MaxOutputBytes: 1 << 16}}
	res, _ := b.Run(context.Background(), Command{Shell: "getent hosts example.com || echo NO-DNS", Surface: "test"}, none)
	if !strings.Contains(res.Output, "NO-DNS") {
		t.Errorf("network none should have no DNS, got %q", res.Output)
	}
}

func TestContainerLive_Reaper(t *testing.T) {
	b := liveContainer(t)
	work := liveWorkDir(t)
	// A command that outlives a short wall-clock deadline must leave no container.
	pol := Policy{WorkDir: work, Network: NetNone, Limits: Limits{Wall: 2 * time.Second, MaxOutputBytes: 1 << 16}}
	res, _ := b.Run(context.Background(), Command{Shell: "sleep 60", Surface: "test"}, pol)
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got exit %d", res.ExitCode)
	}
	// Nothing tagged for this instance should remain.
	time.Sleep(500 * time.Millisecond)
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+labelInstance+"="+b.instance).Output()
	if err != nil {
		t.Skipf("could not list containers: %v", err)
	}
	if left := strings.Fields(string(out)); len(left) > 0 {
		t.Errorf("reaper left %d container(s) behind: %v", len(left), left)
	}
}

// The full escape suite (escapecore.Evaluate) runs against this backend from
// `tenant sandbox selftest` and from escapecore's own live test; it cannot run
// here because escapecore imports this package.
