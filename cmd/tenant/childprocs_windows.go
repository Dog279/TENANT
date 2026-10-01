//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows doesn't tie a child to its parent: a hard kill of tenant.exe (a
// service stop, Task Manager, taskkill /F) left its headless Chrome tree, and
// any other child, running (TEN-401). Tenant joins a job that kills everything
// in it when its last handle closes. The handle is held for the process's
// life, so the OS reaps the tree however tenant.exe ends. A child that must
// outlive Tenant (the user's browser for a sign-in) breaks away.
var childJob windows.Handle

func killChildrenOnExit() {
	job, err := windows.CreateJobObject(nil, nil)
	if err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
		if _, err = windows.SetInformationJobObject(job, uint32(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err == nil {
			err = windows.AssignProcessToJobObject(job, windows.CurrentProcess())
		}
		if err != nil {
			windows.CloseHandle(job)
		} else {
			childJob = job
		}
	}
	if err != nil {
		sysUnavailable.Log(context.Background(), "component", "the child-process job",
			"effect", "a hard kill can leave the web browser running", "err", err)
	}
	sweepChromeProfiles(os.TempDir(), chromeHoldsProfile)
}

// chromeHoldsProfile: a running Chrome keeps dir\lockfile open without
// delete sharing, so removing it fails while that Chrome lives.
func chromeHoldsProfile(dir string) bool {
	err := os.Remove(filepath.Join(dir, "lockfile"))
	return err != nil && !errors.Is(err, os.ErrNotExist)
}

// breakAway lets cmd outlive Tenant instead of dying with its job.
func breakAway(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
}
