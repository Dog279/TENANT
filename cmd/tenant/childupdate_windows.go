//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// childNoWindow gives a process a child starts a console with no window. A
// child under the service manager has no console, and Windows would open a
// window for every console program it starts.
func childNoWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// childReplace gives this process's place to the binary now at exe. Windows
// has no exec: a child an older process keeps exits and is started again by
// it; any other becomes the keeper of the new one, which the service manager
// and the logon task go on seeing as this process.
func childReplace(ctx context.Context, exe, dir string, kept bool, log *slog.Logger) error {
	if kept {
		return exitError{code: childRestartCode}
	}
	return childKeep(ctx, exe, dir, log)
}

// childKeep runs the child at exe as `child run --kept` until ctx ends,
// which it passes on by closing the child's standard input. A child that
// exits with childRestartCode is started again, from whatever binary is at
// exe by then: after its own update, or after it put the old one back. Any
// other end is this process's end, with the same code. The keeper holds
// nothing of the child's (no link, no audit log, no tools) and is in this
// process's job, so the child dies with it however it ends.
func childKeep(ctx context.Context, exe, dir string, log *slog.Logger) error {
	for first := true; ; first = false {
		p, err := startChildProc(exe, dir, "--kept")
		if err != nil && first {
			return fmt.Errorf("%w: %v", errNotReplaced, err)
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			p.stop()
			return nil
		case <-p.done:
		}
		p.stop()
		switch p.code() {
		case 0: // stopped by itself: revoked
			return nil
		case childRestartCode:
		default:
			log.Warn("fleet child: the child this process kept has ended", "how", p.result())
			return exitError{code: max(1, p.code()), msg: "the fleet child ended: " + p.result()}
		}
		log.Info("fleet child: starting the child again, as it asked")
	}
}
