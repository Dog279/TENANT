package procprotect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows. A process and each of its threads have an access list, and the
// default gives the owner, any program of the same user, everything: read
// the memory, write a dump, attach a debugger, redirect a thread. Protect
// replaces the lists with restrictedSDDL: SYSTEM and Administrators keep
// full access; the owner keeps three rights, 0x101001:
//
//	process: end it, wait for it, read its basic details (its path and
//	         command line), which is what Task Manager needs;
//	thread:  end it, wait for it, resume it.
//
// The owner entry names OWNER RIGHTS. Without one, Windows lets an object's
// owner read and rewrite its access list whatever the list says.
//
// Still readable: the command line, the program's path and the account.
// Not stopped: an elevated administrator, SYSTEM, and a program that was
// handed a handle before Protect ran (whoever started Tenant).
const restrictedSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x101001;;;OW)"

const leavesThreads = "another program running as the same user can open one of Tenant's threads and redirect it (SetThreadContext, QueueUserAPC), which runs code inside Tenant and so reads its memory"

func protect() []Control {
	sd, err := windows.SecurityDescriptorFromString(restrictedSDDL)
	set := func(do func() error) func() error {
		return func() error {
			if err != nil {
				return err
			}
			return do()
		}
	}
	return []Control{
		// The threads first: a process whose own list is closed but whose
		// threads are open is not closed.
		apply(Control{ID: "windows.thread-dacl",
			Name:   "opening or redirecting a thread refused (every thread's access list, and the default for new ones)",
			Leaves: leavesThreads,
		}, set(func() error { return restrictThreads(sd) })),
		apply(Control{ID: "windows.process-dacl",
			Name:   "reading memory, writing a dump and attaching a debugger refused (the process's access list)",
			Leaves: "another program running as the same user can read Tenant's memory (ReadProcessMemory), write a dump of it (Task Manager's \"Create dump file\"), attach a debugger and start a thread in it",
		}, set(func() error {
			return windows.SetKernelObjectSecurity(windows.CurrentProcess(), windows.DACL_SECURITY_INFORMATION, sd)
		})),
		apply(Control{ID: "windows.wer-exclusion",
			Name:   "no crash dump by Windows Error Reporting (the program is on the user's exclusion list)",
			Leaves: "Windows Error Reporting, which runs as SYSTEM, can write a dump of Tenant's memory when it crashes",
		}, excludeFromErrorReporting),
	}
}

type tokenDefaultDACL struct{ DefaultDacl *windows.ACL }

// tokenDefaultDACLOf reads the access list t gives new objects.
func tokenDefaultDACLOf(t windows.Token) (*windows.ACL, error) {
	var n uint32
	_ = windows.GetTokenInformation(t, windows.TokenDefaultDacl, nil, 0, &n)
	if n == 0 {
		return nil, errors.New("the token has no default access list to read")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(t, windows.TokenDefaultDacl, &buf[0], n, &n); err != nil {
		return nil, err
	}
	return (*tokenDefaultDACL)(unsafe.Pointer(&buf[0])).DefaultDacl, nil // it points into buf, and keeps it
}

// restrictThreads gives every thread of this process the restricted list:
// the ones the Go runtime creates from now on, through the token's default
// for new objects, and the ones that exist, one by one.
//
// The default is changed first, so no thread can start in between with the
// old list. Before it is changed, the ordinary token and list are kept for
// Child and Ordinary (spawn_windows.go): if they cannot be kept, the default
// is left alone, because with it changed and no way back Tenant could not
// start a command at all.
func restrictThreads(sd *windows.SECURITY_DESCRIPTOR) error {
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ADJUST_DEFAULT, &tok); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer tok.Close()
	if ordinaryToken == 0 {
		if err := keepOrdinary(tok); err != nil {
			return fmt.Errorf("keeping the ordinary access list for the commands Tenant starts: %w", err)
		}
	}
	def := tokenDefaultDACL{DefaultDacl: dacl}
	if err := windows.SetTokenInformation(tok, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&def)), uint32(unsafe.Sizeof(def))); err != nil {
		return fmt.Errorf("SetTokenInformation(TokenDefaultDacl): %w", err)
	}

	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("listing the threads: %w", err)
	}
	defer windows.CloseHandle(snap)
	pid := windows.GetCurrentProcessId()
	var failed []string
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		h, err := windows.OpenThread(windows.WRITE_DAC, false, te.ThreadID)
		switch {
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			// Its list already refuses its owner: it started after the
			// default changed.
			continue
		case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
			continue // it has ended
		case err == nil:
			err = windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, sd)
			windows.CloseHandle(h)
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("thread %d: %v", te.ThreadID, err))
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// keepOrdinary keeps, from before the default changes: a primary copy of the
// token for the processes Tenant starts, an impersonation copy for the thread
// that makes their pipes, and the default access list itself.
func keepOrdinary(tok windows.Token) error {
	acl, err := tokenDefaultDACLOf(tok)
	if err != nil {
		return err
	}
	abs, err := windows.NewSecurityDescriptor()
	if err != nil {
		return err
	}
	if err := abs.SetDACL(acl, true, false); err != nil {
		return err
	}
	sd, err := abs.ToSelfRelative() // a copy of its own
	if err != nil {
		return err
	}
	var primary, thread windows.Token
	if err := windows.DuplicateTokenEx(tok, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	if err := windows.DuplicateTokenEx(tok, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &thread); err != nil {
		primary.Close()
		return fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	ordinarySD, ordinaryToken, ordinaryThread = sd, primary, thread
	return nil
}

// excludeFromErrorReporting puts this program's file name on the user's list
// of programs Windows Error Reporting leaves alone
// (HKCU\Software\Microsoft\Windows\Windows Error Reporting\ExcludedApplications).
// The list is by file name and it stays: every later run of a program of
// that name, by this user, is excluded too.
func excludeFromErrorReporting() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(filepath.Base(exe))
	if err != nil {
		return err
	}
	add := windows.NewLazySystemDLL("wer.dll").NewProc("WerAddExcludedApplication")
	if err := add.Find(); err != nil {
		return err
	}
	if hr, _, _ := add.Call(uintptr(unsafe.Pointer(name)), 0 /* this user only */); int32(hr) < 0 {
		return fmt.Errorf("WerAddExcludedApplication: HRESULT %#x", uint32(hr))
	}
	return nil
}

// system says whether this process is elevated: an elevated administrator's
// other elevated programs are let in by the access list itself.
func system() []string {
	if windows.GetCurrentProcessToken().IsElevated() {
		return []string{"this process is elevated: other elevated programs and SYSTEM keep full access to it, by design"}
	}
	return []string{"this process is not elevated: the user's other programs, which are not elevated either, are the ones refused"}
}
