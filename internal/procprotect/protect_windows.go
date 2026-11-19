package procprotect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Windows. A process and each of its threads have an access list, and the
// default gives the owner, any program of the same user, everything: read
// the memory, write a dump, attach a debugger, redirect a thread. Protect
// replaces the lists with restrictedSDDL: SYSTEM and Administrators keep
// full access; the owner keeps three rights, 0x101001:
//
//	process: end it, wait for it, read its basic details (its path and
//	         command line), which is what Task Manager needs;
//	thread:  end it, wait for it, resume it. A program of the same user
//	         can therefore still end single threads of Tenant, which stops
//	         work and reads nothing.
//
// The owner entry names OWNER RIGHTS. Without one, Windows lets an object's
// owner read and rewrite its access list whatever the list says.
//
// Still readable: the command line, the program's path and the account.
// Not stopped: an elevated administrator and SYSTEM; whoever started Tenant,
// who holds a handle from before Protect ran, and any program that copies
// that handle out of it (a shell waiting for the command is such a holder);
// and a program that opens the process in the moment between its start and
// Protect. The console host's handle, which is of that kind, is replaced
// (console_windows.go).
const restrictedSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x101001;;;OW)"

// tokenSDDL is the access list of the process's token, an object of its own
// that anyone who may read the process's basic details can ask for. The token
// holds the default list new threads get, so an owner who could write the
// token could put the old default back. The owner keeps three rights,
// 0x2000a, none of which changes the token:
//
//	read it (TOKEN_QUERY): Task Manager's "User name";
//	copy it (TOKEN_DUPLICATE): Windows does that inside the process to
//	    answer "is this user in that group" (CheckTokenMembership), and the
//	    copy gives another program of the same user nothing it does not have;
//	read its access list (READ_CONTROL): the console host opens a client's
//	    token with all three (TOKEN_READ) to see what kind of program it is,
//	    and one it cannot read it treats as a sandboxed app, which may not
//	    type into its own console or read its screen back.
const tokenSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x2000a;;;OW)"

var (
	procNtGetNextThread = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtGetNextThread")
	procGetThreadId     = kernel32.NewProc("GetThreadId")
)

// ownToken is this process's token, opened once, before its access list is
// closed: the handle keeps the rights it was opened with.
var ownToken windows.Token

const leavesThreads = "another program running as the same user can open one of Tenant's threads and redirect it (SetThreadContext, QueueUserAPC), which runs code inside Tenant and so reads its memory"

func protect() []Control {
	if len(os.Args) == 2 && os.Args[1] == anchorArg {
		anchor() // the helper that keeps a console open: it does nothing else, and exits
	}
	sd, err := windows.SecurityDescriptorFromString(restrictedSDDL)
	set := func(do func() error) func() error {
		return func() error {
			if err != nil {
				return err
			}
			return do()
		}
	}
	// The threads first: a process whose own list is closed but whose
	// threads are open is not closed.
	threads := apply(Control{ID: "windows.thread-dacl",
		Name:   "reading or redirecting a thread refused (every thread's access list, and the default for new ones); the owner can still end, wait for and resume a thread",
		Leaves: leavesThreads,
	}, set(func() error { return restrictThreads(sd) }))
	process := apply(Control{ID: "windows.process-dacl",
		Name:   "reading memory, writing a dump and attaching a debugger refused (the process's access list)",
		Leaves: "another program running as the same user can read Tenant's memory (ReadProcessMemory), write a dump of it (Task Manager's \"Create dump file\"), attach a debugger and start a thread in it",
	}, set(func() error {
		return windows.SetKernelObjectSecurity(windows.CurrentProcess(), windows.DACL_SECURITY_INFORMATION, sd)
	}))
	// The console host opens the process again only once its list is closed.
	var note string
	console := apply(Control{ID: "windows.console-host",
		Name:   "the console host's handle to this process carries the owner's three rights and no more (the console is left and joined again once the access list is set)",
		Leaves: "another program running as the same user can copy the full-access handle the console host (conhost.exe) holds on Tenant, and read Tenant's memory through it",
	}, func() (err error) {
		if !process.OK {
			return errors.New("the process's access list is not the restricted one, so the console host would open the process with everything again")
		}
		note, err = rejoinConsole()
		return err
	})
	console.Note = note
	wasMissing := false
	wer := apply(Control{ID: "windows.wer-exclusion",
		Name:   "no crash dump by Windows Error Reporting (the program is on the user's exclusion list)",
		Leaves: "Windows Error Reporting, which runs as SYSTEM, can write a dump of Tenant's memory when it crashes",
	}, func() error {
		found, _ := reread("windows.wer-exclusion")
		if wasMissing = found != ""; !wasMissing {
			return nil // on the list already: nothing to add, and wer.dll is not loaded for it
		}
		return excludeFromErrorReporting()
	})
	if wer.OK && wasMissing {
		wer.Note = ExclusionPutBack
	}
	return []Control{threads, process, console, wer}
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
// old list, and the token that holds it is then closed to its owner
// (tokenSDDL). Before it is changed, the ordinary token and list are kept for
// Child and Ordinary (spawn_windows.go): if they cannot be kept, the default
// is left alone, because with it changed and no way back Tenant could not
// start a command at all.
func restrictThreads(sd *windows.SECURITY_DESCRIPTOR) error {
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	tokenSD, err := windows.SecurityDescriptorFromString(tokenSDDL)
	if err != nil {
		return err
	}
	if ownToken == 0 {
		if err := windows.OpenProcessToken(windows.CurrentProcess(),
			windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ADJUST_DEFAULT|windows.WRITE_DAC, &ownToken); err != nil {
			return fmt.Errorf("OpenProcessToken: %w", err)
		}
	}
	tok := ownToken
	if ordinaryToken == 0 {
		if err := keepOrdinary(tok); err != nil {
			return fmt.Errorf("keeping the ordinary access list for the commands Tenant starts: %w", err)
		}
	}
	def := tokenDefaultDACL{DefaultDacl: dacl}
	if err := windows.SetTokenInformation(tok, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&def)), uint32(unsafe.Sizeof(def))); err != nil {
		return fmt.Errorf("SetTokenInformation(TokenDefaultDacl): %w", err)
	}
	if err := windows.SetKernelObjectSecurity(windows.Handle(tok), windows.DACL_SECURITY_INFORMATION, tokenSD); err != nil {
		return fmt.Errorf("the token's own access list: %w", err)
	}

	// This process's threads, one after the other (NtGetNextThread), each
	// opened with whatever its list grants: a list of every thread on the
	// system, which is what a Toolhelp snapshot reads, cost most of the
	// start-up time protection adds (about 55 ms of 60).
	var causes, ids []string
	var h windows.Handle
	for {
		var next windows.Handle
		st, _, _ := procNtGetNextThread.Call(uintptr(windows.CurrentProcess()), uintptr(h), windows.MAXIMUM_ALLOWED, 0, 0, uintptr(unsafe.Pointer(&next)))
		if h != 0 {
			windows.CloseHandle(h)
		}
		if windows.NTStatus(st) == windows.STATUS_NO_MORE_ENTRIES {
			break
		}
		if st != 0 {
			return fmt.Errorf("listing the threads: NtGetNextThread: %w", windows.NTStatus(st))
		}
		h = next
		err := windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, sd)
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			// Its list already refuses its owner WRITE_DAC: it started
			// after the default changed.
			continue
		}
		if err != nil {
			if !slices.Contains(causes, err.Error()) {
				causes = append(causes, err.Error())
			}
			id, _, _ := procGetThreadId.Call(uintptr(h))
			ids = append(ids, strconv.FormatUint(uint64(id), 10))
		}
	}
	if len(causes) > 0 {
		// The cause is the reason; which threads, and how many, differ at
		// every start and are the detail.
		slices.Sort(causes)
		return &failure{reason: "a thread that existed at start could not be given the access list: " + strings.Join(causes, "; "),
			detail: "thread ids " + strings.Join(ids, ", ")}
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

// werExcluded is where the user's exclusion list is kept, a value per
// program name.
const werExcluded = `Software\Microsoft\Windows\Windows Error Reporting\ExcludedApplications`

// reread reads a control's state again, for Now: found says what is wrong
// with it, and whole that everything the control covers was read.
//
//   - The process's access list, and the one new threads get, are read and
//     held to restrictedSDDL's terms. A thread's own list is closed to this
//     process as it is to any program of the user, so the threads that exist
//     are as they were at start.
//   - The exclusion from Windows Error Reporting is a value in the user's
//     registry, which any program of the user can delete.
//   - The handle the console host holds is the console host's, and is not
//     looked for again.
func reread(id string) (found string, whole bool) {
	switch id {
	case "windows.process-dacl":
		sd, err := windows.GetSecurityInfo(windows.CurrentProcess(), windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return "", false
		}
		if dacl, _, err := sd.DACL(); err != nil || !closedToTheUser(dacl) {
			found = "the process's access list gives the user's other programs more than the three rights"
		}
		return found, true
	case "windows.thread-dacl":
		if dacl, err := tokenDefaultDACLOf(ownToken); err == nil && !closedToTheUser(dacl) {
			found = "the access list a new thread gets gives the user's other programs more than the three rights"
		}
		return found, false
	case "windows.wer-exclusion":
		exe, err := os.Executable()
		if err != nil {
			return "", false
		}
		k, err := registry.OpenKey(registry.CURRENT_USER, werExcluded, registry.QUERY_VALUE)
		if err == nil {
			defer k.Close()
			if v, _, err := k.GetIntegerValue(filepath.Base(exe)); err == nil && v != 0 {
				return "", true
			}
		}
		return "this program is not on the user's Windows Error Reporting exclusion list", true
	}
	return "", false
}

// closedToTheUser: the access list gives SYSTEM and Administrators what it
// likes, the owner no more than the three rights, and nobody else anything.
func closedToTheUser(dacl *windows.ACL) bool {
	if dacl == nil { // no list at all lets everyone in
		return false
	}
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	admins, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	owner, _ := windows.CreateWellKnownSid(windows.WinCreatorOwnerRightsSid)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		switch sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); {
		case sid.Equals(system), sid.Equals(admins):
		case sid.Equals(owner) && ace.Mask&^0x101001 == 0:
		default:
			return false
		}
	}
	return true
}

// A process is elevated or it is not: system's line is about this one.
const systemIsOfTheProcess = true

// system says whether this process is elevated: an elevated administrator's
// other elevated programs are let in by the access list itself.
func system() []string {
	if windows.GetCurrentProcessToken().IsElevated() {
		return []string{"this process is elevated: other elevated programs and SYSTEM keep full access to it, by design"}
	}
	return []string{"this process is not elevated: the user's other programs, which are not elevated either, are the ones refused"}
}
