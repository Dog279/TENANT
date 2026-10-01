//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TEN-401 acceptance: hard-kill a process that joined the kill-on-close job
// (TerminateProcess, as taskkill /F and a service stop do) and its child dies
// with it.
func TestKillChildrenOnExit_HardKillReapsChild(t *testing.T) {
	if os.Getenv("TENANT_JOB_HELPER") == "1" {
		killChildrenOnExit()
		child := exec.Command("ping", "-n", "120", "127.0.0.1")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Printf("CHILD %d\n", child.Process.Pid)
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestKillChildrenOnExit_HardKillReapsChild$")
	helper.Env = append(os.Environ(), "TENANT_JOB_HELPER=1")
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := 0
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "CHILD "); ok {
			pid, _ = strconv.Atoi(rest)
			break
		}
	}
	if pid == 0 {
		_ = helper.Process.Kill()
		t.Fatal("helper never reported its child")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		_ = helper.Process.Kill()
		t.Fatalf("open child %d: %v", pid, err)
	}
	defer windows.CloseHandle(h)

	_ = helper.Process.Kill() // TerminateProcess: no cleanup runs
	_ = helper.Wait()
	if ev, _ := windows.WaitForSingleObject(h, 10_000); ev != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(h, 1)
		t.Fatal("the child outlived its hard-killed parent")
	}
}
