//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"tenant/internal/procprotect"
)

// processesOf lists the other processes whose program is the file at exe,
// each with its command line. Linux says which file a process runs in /proc;
// elsewhere ps does.
func processesOf(exe string) (out []string) {
	want, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir("/proc")
	if err != nil || len(ents) == 0 {
		return processesByPS(want)
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() || !sameFile(want, "/proc/"+e.Name()+"/exe") {
			continue
		}
		args, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		out = append(out, strings.TrimSpace(fmt.Sprintf("pid %d  %s", pid, bytes.ReplaceAll(bytes.TrimRight(args, "\x00"), []byte{0}, []byte{' '}))))
	}
	return out
}

func sameFile(want os.FileInfo, path string) bool {
	fi, err := os.Stat(path)
	return err == nil && os.SameFile(want, fi)
}

// ps runs ps itself and not runCmd: runCmd is the way to the service manager,
// which the tests close, and a list of processes changes nothing.
func ps(args ...string) string {
	out, _ := procprotect.CombinedOutput(exec.Command("ps", args...))
	return string(out)
}

// processesByPS is processesOf where there is no /proc (macOS). Each column
// has its own -o: FreeBSD's ps takes all that follows the first "=" as that
// column's heading. -ww: no cut at a line width, which some apply to a pipe.
func processesByPS(want os.FileInfo) (out []string) {
	listing := ps("-axww", "-o", "pid=", "-o", "comm=")
	for _, line := range strings.Split(listing, "\n") {
		num, comm, _ := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(num)
		if err != nil || pid == os.Getpid() {
			continue
		}
		// ponytail: comm is the name the process was started under, a path or
		// a bare name. A bare name is looked up on this account's PATH; a
		// process started by a relative path from another folder is not found.
		// The kernel's own record of the file (kern.procargs2) is the upgrade.
		if comm = strings.TrimSpace(comm); !strings.Contains(comm, "/") {
			comm, _ = exec.LookPath(comm)
		}
		if !sameFile(want, comm) {
			continue
		}
		out = append(out, strings.TrimSpace(fmt.Sprintf("pid %d  %s", pid, strings.TrimSpace(ps("-ww", "-o", "args=", "-p", num)))))
	}
	return out
}

// tasksOf: only Windows has scheduled tasks. A unit or an agent of the
// operator's own is not looked for; the processes' command lines say what
// they are.
func tasksOf(context.Context, string) []string { return nil }
