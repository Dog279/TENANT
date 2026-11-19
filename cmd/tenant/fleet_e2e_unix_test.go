//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// e2eQuiet is Windows' concern (no console window, below-normal priority):
// nothing opens a window here.
func e2eQuiet(*exec.Cmd) {}

// e2eProtected is nil when another program of this user is refused this
// process's environment, as TEN-535's non-dumpable mark refuses it on Linux
// (/proc/<pid> is root's then). macOS and root are not checked.
func e2eProtected(pid string) error {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		return nil
	}
	if _, err := os.ReadFile("/proc/" + pid + "/environ"); !os.IsPermission(err) {
		return fmt.Errorf("its /proc/%s/environ can be read (%v)", pid, err)
	}
	return nil
}

// selfUsage is what this process has cost so far: its CPU time (user and
// system) and the memory it holds resident. On Linux that is the resident
// set now; on macOS, which has no /proc, the largest it has been.
func selfUsage() (cpu time.Duration, rss int64) {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		cpu = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
		if runtime.GOOS == "darwin" {
			rss = int64(ru.Maxrss) // bytes there
		}
	}
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 1 {
			if pages, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				rss = pages * int64(os.Getpagesize())
			}
		}
	}
	return cpu, rss
}
