//go:build windows

package main

import (
	"errors"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// e2eQuiet starts a process of the end-to-end tests with no console window
// and at below-normal priority: somebody is using this computer.
func e2eQuiet(cmd *exec.Cmd) {
	hideWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.BELOW_NORMAL_PRIORITY_CLASS
}

// e2eProtected is nil when another program of this user is refused this
// process's memory, as TEN-535's access list refuses it.
func e2eProtected(pid string) error {
	n, err := strconv.ParseUint(pid, 10, 32)
	if err != nil {
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_VM_READ, false, uint32(n))
	if err == nil {
		windows.CloseHandle(h)
		return errors.New("it can be opened to read its memory")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	return nil
}

var procGetProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// selfUsage is what this process has cost so far: its CPU time (user and
// kernel) and the memory it holds resident now (its working set).
func selfUsage() (cpu time.Duration, rss int64) {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user) == nil {
		ticks := int64(kernel.HighDateTime)<<32 | int64(kernel.LowDateTime)
		ticks += int64(user.HighDateTime)<<32 | int64(user.LowDateTime)
		cpu = time.Duration(ticks) * 100 // a tick is 100 ns
	}
	// PROCESS_MEMORY_COUNTERS
	var mem struct {
		cb                         uint32
		pageFaultCount             uint32
		peakWorkingSetSize         uintptr
		workingSetSize             uintptr
		quotaPeakPagedPoolUsage    uintptr
		quotaPagedPoolUsage        uintptr
		quotaPeakNonPagedPoolUsage uintptr
		quotaNonPagedPoolUsage     uintptr
		pagefileUsage              uintptr
		peakPagefileUsage          uintptr
	}
	mem.cb = uint32(unsafe.Sizeof(mem))
	if ok, _, _ := procGetProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&mem)), uintptr(mem.cb)); ok != 0 {
		rss = int64(mem.workingSetSize)
	}
	return cpu, rss
}
