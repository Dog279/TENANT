package procprotect_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"tenant/internal/plugins/web"
	"tenant/internal/procprotect"
	"tenant/internal/sandbox"
)

// What Tenant starts must still start, and still work, from a process that
// has called Protect: the agent's commands and a local tool server, on the
// code that runs them (internal/sandbox), and the browser
// (internal/plugins/web). Each test runs itself again as a process that
// calls Protect first, as main does.

const envProtected = "PROCPROTECT_PROTECTED"

// protected reports whether this is the second run, after calling Protect in
// it; the first run starts the second and reports how it went.
func protected(t *testing.T) bool {
	t.Helper()
	if procprotect.Disabled {
		t.Skip("built with -tags debugattach")
	}
	if os.Getenv(envProtected) != "" {
		if m := procprotect.Protect().Missing(); len(m) > 0 {
			t.Fatalf("controls not in force: %+v", m)
		}
		return true
	}
	c := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	c.Env = append(os.Environ(), envProtected+"=1")
	out, err := procprotect.CombinedOutput(c)
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("in a protected process: %v\n%s", err, out)
	}
	return false
}

func TestAgentCommandsRunFromAProtectedProcess(t *testing.T) {
	if !protected(t) {
		return
	}
	// A command that makes a pipe of its own: on Windows that is what a
	// process started without the user's ordinary token cannot do.
	pipeline, want := "echo hi | cat", "hi"
	backends := []string{"host"}
	if runtime.GOOS == "windows" {
		pipeline = "cmd /c 'echo hi | findstr hi'"
		// The Job Object backend opens the process it started, and its first
		// thread, by id: the restricted access list would refuse both.
		backends = append(backends, "jobobject")
	}
	for _, name := range backends {
		be, err := sandbox.For(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		res, err := be.Run(context.Background(), sandbox.Command{Shell: pipeline}, sandbox.Policy{Limits: sandbox.Limits{Wall: time.Minute}})
		if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Output) != want {
			t.Errorf("%s: Run = %q, exit %d, %v; want %q", name, res.Output, res.ExitCode, err, want)
		}
		// A long-lived process with open pipes, as a local MCP server is.
		p, err := sandbox.Start(context.Background(), be, sandbox.ShellArgv(pipeline), sandbox.Policy{})
		if err != nil {
			t.Errorf("%s: Start: %v", name, err)
			continue
		}
		p.Stdin().Close()
		out, _ := io.ReadAll(p.Stdout())
		if err := p.Wait(); err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("%s: Start = %q, %v; want %q", name, out, err, want)
		}
	}
}

// Needs a local Chrome or Edge, so it is gated like the browser's own test:
//
//	TENANT_CHROME=1 go test ./internal/procprotect -run TestBrowserStartsFromAProtectedProcess -v
func TestBrowserStartsFromAProtectedProcess(t *testing.T) {
	if os.Getenv("TENANT_CHROME") == "" {
		t.Skip("set TENANT_CHROME=1 to start a real Chrome")
	}
	if !protected(t) {
		return
	}
	// The browser must not outlive this process, however it ends.
	endTreeWithThisProcess(t)
	// NewSession reads Chrome's address from its output, through the pipe,
	// and runs its first commands in it.
	sess, err := web.NewSession(web.Config{Headless: true})
	if err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
}
