//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processesOf lists the other processes whose program is the file at exe,
// each with its command line. Linux says which file a process runs in /proc;
// elsewhere ps does (its comm column is the program's path on macOS).
func processesOf(exe string) (out []string) {
	want, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	same := func(path string) bool {
		fi, err := os.Stat(path)
		return err == nil && os.SameFile(want, fi)
	}
	if ents, err := os.ReadDir("/proc"); err == nil && len(ents) > 0 {
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid == os.Getpid() || !same("/proc/"+e.Name()+"/exe") {
				continue
			}
			args, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			out = append(out, strings.TrimSpace(fmt.Sprintf("pid %d  %s", pid, bytes.ReplaceAll(bytes.TrimRight(args, "\x00"), []byte{0}, []byte{' '}))))
		}
		return out
	}
	listing, err := runCmd(context.Background(), "ps", "-axo", "pid=,comm=")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(listing, "\n") {
		num, comm, _ := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(num)
		if err != nil || pid == os.Getpid() || !same(strings.TrimSpace(comm)) {
			continue
		}
		args, _ := runCmd(context.Background(), "ps", "-o", "args=", "-p", num)
		out = append(out, strings.TrimSpace(fmt.Sprintf("pid %d  %s", pid, strings.TrimSpace(args))))
	}
	return out
}

// tasksOf: only Windows has scheduled tasks. A unit or an agent of the
// operator's own is not looked for; the processes' command lines say what
// they are.
func tasksOf(context.Context, string) []string { return nil }
