package shadow

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sqliteTempFiles lists the temporary files SQLite has open. On Windows they
// are in the temporary directory under the name etilqs_*, and go when closed.
func sqliteTempFiles() (names []string, supported bool) {
	entries, _ := os.ReadDir(os.TempDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "etilqs_") {
			names = append(names, e.Name())
		}
	}
	return names, true
}

// processMemory is the memory this process has in RAM (its working set), now and at its
// highest, in bytes.
func processMemory() (now, peak uint64, ok bool) {
	var c struct {
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
	c.cb = uint32(unsafe.Sizeof(c))
	r, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo").
		Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	return uint64(c.workingSetSize), uint64(c.peakWorkingSetSize), r != 0
}
