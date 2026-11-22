//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func mapPages(n int) ([]byte, error) {
	addr, err := windows.VirtualAlloc(0, uintptr(n), windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE)
	if err != nil {
		return nil, fmt.Errorf("VirtualAlloc: %w", err)
	}
	// unsafe.Add from nil, not unsafe.Pointer(addr): the address is not Go
	// memory, and go vet reads the plain conversion as a misuse.
	return unsafe.Slice((*byte)(unsafe.Add(nil, addr)), n), nil
}

func lockPages(mem []byte) (locked bool, reason string) {
	if err := windows.VirtualLock(pagesAddr(mem), uintptr(len(mem))); err != nil {
		return false, "VirtualLock: " + err.Error()
	}
	return true, ""
}

// unmapPages unlocks only what was locked: VirtualUnlock on pages that are
// not locked fails, and trims them from the working set.
func unmapPages(mem []byte, locked bool) error {
	var unlockErr error
	if locked {
		unlockErr = windows.VirtualUnlock(pagesAddr(mem), uintptr(len(mem)))
	}
	return errors.Join(unlockErr, windows.VirtualFree(pagesAddr(mem), 0, windows.MEM_RELEASE))
}

func pagesAddr(mem []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(mem))) }
