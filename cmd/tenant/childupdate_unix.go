//go:build !windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
)

func childNoWindow(*exec.Cmd) {}

// childReplace gives this process's place to the binary now at exe: it
// execs it, with the command line the units run, so the process the service
// manager watches carries on as the new version. Every file this process
// opened closes on exec, the audit log's lock with it. It returns only when
// the exec failed.
func childReplace(_ context.Context, exe, dir string, _ bool, _ *slog.Logger) error {
	cmd := childCommand(context.Background(), exe, "child", "run", "--dir", dir)
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	return fmt.Errorf("%w: exec %s: %v", errNotReplaced, cmd.Path, syscall.Exec(cmd.Path, cmd.Args, env))
}
