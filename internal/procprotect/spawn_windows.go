package procprotect

import (
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Protect makes the restricted access list the default for every object this
// process creates, which is how the Go runtime's threads get it
// (protect_windows.go). The default lands on more than threads, and two of
// those have to be ordinary again:
//
//   - A process Tenant starts would get the restricted list, and its token
//     would carry the restricted default, so it could not open a pipe it had
//     just made: cmd's "a | b", PowerShell and node all failed that way.
//     Child gives it the user's ordinary token and an ordinary access list.
//   - A pipe os/exec makes (os.Pipe) is created with the default list and
//     then opened by name for writing. The restricted list does not let the
//     owner do that, so every os.Pipe failed with "Access is denied" for a
//     user who is not an administrator. Ordinary runs a function on a thread
//     that carries the user's ordinary default.
//
// Both are set by Protect. Until it has run, and in a build that compiles
// the controls out, Child and Ordinary change nothing.
var (
	ordinaryToken  windows.Token                // primary: what a child is started with
	ordinaryThread windows.Token                // impersonation: what Ordinary's thread carries
	ordinarySD     *windows.SECURITY_DESCRIPTOR // the access list the user's objects had before Protect
)

// Child prepares cmd to start as an ordinary process of the user: the token
// Tenant itself had before Protect, and an access list given explicitly for
// the process and its first thread. What the caller set already is kept.
func Child(cmd *exec.Cmd) {
	if ordinaryToken == 0 {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	a := cmd.SysProcAttr
	sa := &syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: uintptr(unsafe.Pointer(ordinarySD))} // ordinarySD is never freed
	if a.Token == 0 {
		a.Token = syscall.Token(ordinaryToken)
	}
	if a.ProcessAttributes == nil {
		a.ProcessAttributes = sa
	}
	if a.ThreadAttributes == nil {
		a.ThreadAttributes = sa
	}
}

// Ordinary runs f on a thread whose new objects get the user's ordinary
// access list, so that a pipe made in f can be opened. The thread carries a
// copy of Tenant's own token for the length of f: the same user, groups and
// privileges, and only the default access list differs.
//
// The goroutine is locked to its thread for that time. The Go runtime never
// starts a thread from a locked one (it asks a clean thread of its own to),
// so the threads it creates while f runs still get the restricted list.
func Ordinary(f func()) {
	if ordinaryThread == 0 {
		f()
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var cur windows.Token
	if windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &cur) == nil {
		cur.Close() // already inside Ordinary: its end must not be this one's
		f()
		return
	}
	if err := windows.SetThreadToken(nil, ordinaryThread); err != nil {
		f() // a pipe made in f will be refused; the caller's error says so
		return
	}
	defer func() {
		if windows.RevertToSelf() != nil {
			// Never hand a thread that still carries the token back to the
			// runtime: locked once more, it ends with this goroutine.
			runtime.LockOSThread()
		}
	}()
	f()
}
