package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
)

// `tenant update` with nothing to restart says what still runs the old binary
// and what starts it (TEN-556), or that nothing it can see does.
func TestRestartNotice(t *testing.T) {
	got := restartNotice("0.2.0", "0.3.0", []string{"pid 4312", "pid 5120"}, []string{`\TenantServe  serve --hidden`, `\TenantMCP  mcp-memory`})
	want := "updated 0.2.0 -> 0.3.0; restart Tenant to finish the update. Still running 0.2.0:\n" +
		"  pid 4312\n  pid 5120\n" +
		"Scheduled tasks that start this binary (`tenant service` installed none of them, so it restarts none):\n" +
		"  " + `\TenantServe  serve --hidden` + "\n  " + `\TenantMCP  mcp-memory` + "\n" +
		"Restart each one: schtasks /End /TN \"<task>\", then schtasks /Run /TN \"<task>\"\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := restartNotice("0.2.0", "0.3.0", nil, nil); got != "updated 0.2.0 -> 0.3.0; no process this account can see runs 0.2.0, so the new version is what starts next\n" {
		t.Errorf("with nothing running: %q", got)
	}
}

// processesOf finds another process that runs the same program file, and
// never this one.
func TestProcessesOfFindsAnotherProcessOfTheSameBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := bootHelperCmd("live", t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("the helper did not come up: %q %v", line, err)
	}
	got := processesOf(exe)
	found := false
	for _, p := range got {
		if p == fmt.Sprintf("pid %d", cmd.Process.Pid) || strings.HasPrefix(p, fmt.Sprintf("pid %d ", cmd.Process.Pid)) {
			found = true
		}
		if p == fmt.Sprintf("pid %d", os.Getpid()) || strings.HasPrefix(p, fmt.Sprintf("pid %d ", os.Getpid())) {
			t.Errorf("it lists this process itself: %q", p)
		}
	}
	if !found {
		t.Errorf("processesOf(%s) = %q, want pid %d among them", exe, got, cmd.Process.Pid)
	}
	if got := processesOf(os.DevNull + "-no-such-binary"); len(got) != 0 {
		t.Errorf("for a file that is not there: %q", got)
	}
}
