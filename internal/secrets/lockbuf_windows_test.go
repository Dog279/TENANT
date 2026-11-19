package secrets

import (
	"bytes"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() {
	osLocked = func(addr uintptr) bool { return workingSet(addr)>>22&1 == 1 }
	// The address alone does not tell a leak from a reuse: the moment a region
	// is released, something else in the process can be given the same address
	// (the fuzz test saw it within seconds: a new read-write region of the
	// same size). So the mapping is made read-only before it goes back. A
	// region at that address afterwards is the buffer's own if it is still
	// read-only, or if it is reserved and exactly the buffer's size, which is
	// what a decommit without a release leaves.
	osWatch = func(mem []byte) func() bool {
		addr, page := addrOf(mem), uintptr(os.Getpagesize())
		size := (uintptr(len(mem)) + page - 1) &^ (page - 1)
		var old uint32
		if err := windows.VirtualProtect(addr, size, windows.PAGE_READONLY, &old); err != nil {
			panic(err)
		}
		return func() bool {
			var region windows.MemoryBasicInformation
			if err := windows.VirtualQuery(addr, &region, unsafe.Sizeof(region)); err != nil {
				panic(err)
			}
			return region.AllocationBase == addr &&
				(region.Protect == windows.PAGE_READONLY || region.State == windows.MEM_RESERVE && region.RegionSize == size)
		}
	}
}

// wsBlock is PSAPI_WORKING_SET_EX_INFORMATION. A package variable, so that
// the address handed to QueryWorkingSetEx cannot move.
var wsBlock struct{ addr, attributes uintptr }

// workingSet returns QueryWorkingSetEx's attributes of the page at addr: bit
// 0 is Valid (the page is in the working set), bit 22 is Locked. (The Locked
// method of x/sys's own type compares the masked bit with 1 and is never
// true.)
func workingSet(addr uintptr) uintptr {
	wsBlock.addr, wsBlock.attributes = addr, 0
	if err := windows.QueryWorkingSetEx(windows.CurrentProcess(), uintptr(unsafe.Pointer(&wsBlock)), uint32(unsafe.Sizeof(wsBlock))); err != nil {
		panic(err)
	}
	return wsBlock.attributes
}

func TestLockedBufferWorkingSet(t *testing.T) {
	watchFree(t)
	b, err := NewLockedBuffer([]byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	var addr uintptr
	b.With(func(p []byte) error { addr = addrOf(p); return nil })
	attributes := workingSet(addr)
	t.Logf("QueryWorkingSetEx for the buffer's page: %#x (Valid %d, Locked %d)", attributes, attributes&1, attributes>>22&1)
	if attributes&1 != 1 || attributes>>22&1 != 1 {
		t.Fatalf("want a page that is in the working set and locked; got %#x", attributes)
	}
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
}

// VirtualLock refuses more than the process's minimum working set.
func TestLockedBufferLockRefused(t *testing.T) {
	var minimum, maximum uintptr
	var flags uint32
	windows.GetProcessWorkingSetSizeEx(windows.CurrentProcess(), &minimum, &maximum, &flags)
	if minimum == 0 {
		t.Fatal("no minimum working set size")
	}
	size := int(minimum) * 2
	b, reason := refusedLock(t, bytes.Repeat([]byte("k"), size), "VirtualLock")
	t.Logf("%d bytes against a minimum working set of %d: not locked: %s", size, minimum, reason)
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
}
