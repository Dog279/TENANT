//go:build windows

package main

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// removeTestExclusion takes the test binary's name off the user's Windows
// Error Reporting exclusion list when the test ends: a test whose Tenant
// applies every control for real writes it there.
func removeTestExclusion(t *testing.T, bin string) {
	t.Cleanup(func() {
		p, _ := windows.UTF16PtrFromString(filepath.Base(bin))
		_, _, _ = windows.NewLazySystemDLL("wer.dll").NewProc("WerRemoveExcludedApplication").Call(uintptr(unsafe.Pointer(p)), 0)
	})
}

// doneByDoctorItself is what `tenant doctor` says when its own start put back
// a control it then asks a running Tenant about (putBackBeforeAsking): on
// Windows the exclusion from Windows Error Reporting, kept by file name.
const doneByDoctorItself = "OFF until this doctor started"

// undoFromOutside is how another program of the user undoes one control of a
// running Tenant on Windows, as the tester did: the exclusion from Windows
// Error Reporting is a value in the user's own registry, under the program's
// file name, and anyone running as the user can delete it.
//
// The serve of this test therefore applies every control for real. The value
// it writes is the test binary's name, and it is gone again when the test
// ends.
func undoFromOutside(t *testing.T, bin string) (env []string, undo func(t *testing.T, pid int), found string) {
	const list = `Software\Microsoft\Windows\Windows Error Reporting\ExcludedApplications`
	name := filepath.Base(bin)
	removeTestExclusion(t, bin)
	return nil, func(t *testing.T, _ int) {
		k, err := registry.OpenKey(registry.CURRENT_USER, list, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			t.Fatalf("the user's exclusion list: %v", err)
		}
		defer k.Close()
		if v, _, err := k.GetIntegerValue(name); err != nil || v == 0 {
			t.Fatalf("the serve did not put %s on the exclusion list: %d, %v", name, v, err)
		}
		if err := k.DeleteValue(name); err != nil {
			t.Fatal(err)
		}
	}, "this program is not on the user's Windows Error Reporting exclusion list"
}
