package secrets

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// In a mapping's flags "lo" is locked and "dd" is do not dump. A mapping with
// "dd" is a buffer's, whatever else is given the address later: nothing else
// in a Go program asks for MADV_DONTDUMP.
func init() {
	osLocked = func(addr uintptr) bool {
		flags, _ := smaps(addr)
		return slices.Contains(flags, "lo")
	}
	osWatch = func(mem []byte) func() bool {
		addr := addrOf(mem)
		return func() bool {
			flags, _ := smaps(addr)
			return slices.Contains(flags, "dd")
		}
	}
}

// smaps returns what the kernel reports in /proc/self/smaps for the mapping
// that holds addr: its VmFlags and its "Locked:" size in kB. No flags means
// no mapping holds addr.
func smaps(addr uintptr) (flags []string, lockedKB int) {
	f, err := os.Open("/proc/self/smaps")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	inside := false
	for sc := bufio.NewScanner(f); sc.Scan(); {
		line := sc.Text()
		var start, end uintptr
		if n, _ := fmt.Sscanf(line, "%x-%x ", &start, &end); n == 2 {
			inside = start <= addr && addr < end
		} else if !inside {
			continue
		} else if rest, ok := strings.CutPrefix(line, "Locked:"); ok {
			fmt.Sscanf(rest, "%d", &lockedKB)
		} else if rest, ok := strings.CutPrefix(line, "VmFlags:"); ok {
			return strings.Fields(rest), lockedKB
		}
	}
	return nil, 0
}

func TestLockedBufferSmaps(t *testing.T) {
	watchFree(t)
	b, err := NewLockedBuffer([]byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	var addr uintptr
	b.With(func(p []byte) error { addr = addrOf(p); return nil })
	flags, lockedKB := smaps(addr)
	t.Logf("smaps for the buffer's mapping: Locked: %d kB, VmFlags: %v", lockedKB, flags)
	if !slices.Contains(flags, "lo") || !slices.Contains(flags, "dd") || lockedKB*1024 < os.Getpagesize() {
		t.Fatalf("want a locked (lo), undumpable (dd) mapping with a page locked; got Locked: %d kB, VmFlags: %v", lockedKB, flags)
	}
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
	if flags, _ := smaps(addr); slices.Contains(flags, "dd") || slices.Contains(flags, "lo") {
		t.Fatalf("after Release the mapping is still there: VmFlags: %v", flags)
	}
}

// The kernel refuses mlock to a process whose RLIMIT_MEMLOCK is zero and that
// lacks CAP_IPC_LOCK. Both are arranged in a second process, so that neither
// touches the other tests.
func TestLockedBufferLockRefused(t *testing.T) {
	if os.Getenv("TENANT_LOCKBUF_REFUSED") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLockedBufferLockRefused$", "-test.v")
		cmd.Env = append(os.Environ(), "TENANT_LOCKBUF_REFUSED=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !bytes.Contains(out, []byte("not locked: mlock: ")) {
			t.Fatalf("the second process: %v\n%s", err, out)
		}
		t.Logf("the second process:\n%s", out)
		return
	}
	// Capabilities belong to the thread, so stay on this one.
	runtime.LockOSThread()
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{}); err != nil {
		t.Fatal(err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var caps [2]unix.CapUserData
	if err := unix.Capget(&hdr, &caps[0]); err != nil {
		t.Fatal(err)
	}
	caps[0].Effective &^= 1 << unix.CAP_IPC_LOCK
	if err := unix.Capset(&hdr, &caps[0]); err != nil {
		t.Fatal(err)
	}

	b, reason := refusedLock(t, []byte("still usable"), "mlock")
	b.With(func(p []byte) error {
		flags, lockedKB := smaps(addrOf(p))
		fmt.Printf("uid %d, not locked: %s; smaps Locked: %d kB, VmFlags: %v\n", os.Getuid(), reason, lockedKB, flags)
		if !slices.Contains(flags, "dd") || lockedKB != 0 {
			t.Errorf("want an unlocked mapping still left out of dumps; got Locked: %d kB, VmFlags: %v", lockedKB, flags)
		}
		return nil
	})
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
}
