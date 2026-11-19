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
// replaces the lists with processSDDL and threadSDDL: SYSTEM and
// Administrators keep full access, and the owner keeps a few rights:
//
//	process, 0x101401: end it, wait for it, read its basic details (its
//	         path and command line), which is what Task Manager needs, and
//	         query its information (PROCESS_QUERY_INFORMATION), which is
//	         what taskkill /F, Stop-Process -Force and Node's process.kill open it
//	         with before they end it (decision A3);
//	thread,  0x121001: end it, wait for it, resume it, and read its access
//	         list (READ_CONTROL), so that Protect can read every thread's
//	         list back and check it (decision A80). A program of the same
//	         user can therefore still end single threads of Tenant, which
//	         stops work and reads nothing, and read a thread's access list,
//	         which says nothing of its memory or its registers.
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
const (
	processSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x101401;;;OW)"
	// threadSDDL is also the default for every object the process makes from
	// Protect on (the token's default access list), which is how a thread
	// the Go runtime starts later gets it.
	threadSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x121001;;;OW)"
	// threadReadBack is threadSDDL as a thread carries it: Windows turns the
	// generic right GA into the thread's own rights when the list is set.
	threadReadBack = "D:P(A;;0x1fffff;;;SY)(A;;0x1fffff;;;BA)(A;;0x121001;;;OW)"

	ownerProcessRights = 0x101401
	ownerThreadRights  = 0x121001
)

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
	procNtOpenThread  = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtOpenThread")
	procNtQueryObject = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryObject")
)

// What a thread can be found to be, when it is not as Protect left it. Each
// is the same at every start: the gate matches on the reason they make.
const (
	threadNotClosed  = "its access list, read back, is not the restricted one, and does not let its owner change it"
	threadChanged    = "its access list, read again, is not the restricted one"
	threadUnreadable = "its access list cannot be read back"
	threadHidden     = "a thread does not let this process open it at all, so its access list cannot be checked"
	threadsTooMany   = "the process has more threads than a starting Tenant has, so they were not checked one by one"
)

// maxThreads bounds the threads Protect checks one by one. A Tenant has 4
// to 12 threads when Protect runs, and a busy serve 26 to 29 (round four of
// the independent test); only a program that adds threads to Tenant from
// outside (with a handle from before Protect) gets near it, and that would
// otherwise slow the start without bound.
const maxThreads = 512

// grantedAccess is the access h was opened with (OBJECT_BASIC_INFORMATION).
func grantedAccess(h windows.Handle) (uint32, error) {
	var info [14]uint32 // Attributes, GrantedAccess, and the rest
	if st, _, _ := procNtQueryObject.Call(uintptr(h), 0 /* ObjectBasicInformation */, uintptr(unsafe.Pointer(&info[0])), unsafe.Sizeof(info), 0); st != 0 {
		return 0, fmt.Errorf("NtQueryObject: %w", windows.NTStatus(st))
	}
	return info[1], nil
}

// systemThread is SYSTEM_THREAD_INFORMATION, which follows each process in
// the system's list of processes, one per thread.
type systemThread struct {
	KernelTime, UserTime, CreateTime int64
	WaitTime                         uint32
	StartAddress                     uintptr
	UniqueProcess, UniqueThread      uintptr // CLIENT_ID
	Priority, BasePriority           int32
	ContextSwitches                  uint32
	ThreadState, WaitReason          uint32
}

// ownThreadIDs lists this process's threads by id, from the system's list of
// processes: a list that does not depend on what the threads' own access
// lists let this process open. About 7 ms.
func ownThreadIDs() ([]uintptr, error) {
	buf := make([]byte, 1<<20)
	for {
		var n uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &n)
		if err == windows.STATUS_INFO_LENGTH_MISMATCH {
			buf = make([]byte, n+1<<16)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("NtQuerySystemInformation: %w", err)
		}
		break
	}
	pid := uintptr(windows.GetCurrentProcessId())
	for off := uintptr(0); off < uintptr(len(buf)); {
		p := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
		if p.UniqueProcessID == pid {
			first := off + unsafe.Sizeof(*p)
			if first+uintptr(p.NumberOfThreads)*unsafe.Sizeof(systemThread{}) > uintptr(len(buf)) {
				return nil, errors.New("the system's list of this process's threads is cut short")
			}
			ids := make([]uintptr, p.NumberOfThreads)
			for i, th := range unsafe.Slice((*systemThread)(unsafe.Pointer(&buf[first])), p.NumberOfThreads) {
				if th.UniqueProcess != pid {
					return nil, errors.New("the system's list of this process's threads names another process")
				}
				ids[i] = th.UniqueThread
			}
			return ids, nil
		}
		if p.NextEntryOffset == 0 {
			break
		}
		off += uintptr(p.NextEntryOffset)
	}
	return nil, errors.New("this process is not in the system's list of processes")
}

// openThread opens this process's thread id with whatever its access list
// grants this process. Naming the process as well as the thread, the system
// refuses an id that is not a thread of this process (STATUS_INVALID_CID),
// so an id freed and given to another process's thread meanwhile is never
// opened.
func openThread(id uintptr) (windows.Handle, windows.NTStatus) {
	var h windows.Handle
	oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{}))}
	cid := [2]uintptr{uintptr(windows.GetCurrentProcessId()), id}
	st, _, _ := procNtOpenThread.Call(uintptr(unsafe.Pointer(&h)), windows.MAXIMUM_ALLOWED, uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(&cid)))
	return h, windows.NTStatus(st)
}

// checkThreads holds every thread the system lists for this process to
// threadReadBack, by its id, so the threads checked are exactly the threads
// listed (decision A80, and round four of the independent test: a count of
// the threads a walk reached could be made to agree by ending one).
//
// A thread whose list may be changed is first given set (Protect; nil only
// reads, for Now). Each is then opened by its id, and:
//   - opened: its list is read back and compared entry by entry;
//   - STATUS_INVALID_CID: no thread of this process has that id any more,
//     which only a thread that has ended can be (a live thread keeps its id),
//     and an ended thread runs nothing;
//   - refused, or anything else: the thread is there and cannot be checked,
//     so the control is off. A hidden thread cannot pass for an ended one.
//
// A thread that starts after the list was taken is not in it; it gets the
// restricted list from the token's default, which is set before this runs.
// More than limit threads (0: no limit) is off too, without opening any.
func checkThreads(set *windows.SECURITY_DESCRIPTOR, limit int) error {
	want, err := windows.SecurityDescriptorFromString(threadReadBack)
	if err != nil {
		return err
	}
	wantACL, _, err := want.DACL()
	if err != nil {
		return err
	}
	list, err := ownThreadIDs()
	if err != nil {
		return err
	}
	if limit > 0 && len(list) > limit {
		return &failure{reason: threadsTooMany, detail: strconv.Itoa(len(list)) + " threads"}
	}
	mismatch := threadChanged
	if set != nil {
		mismatch = threadNotClosed
	}
	var causes, ids []string
	for _, id := range list {
		cause := ""
		h, st := openThread(id)
		switch {
		case st == windows.STATUS_INVALID_CID:
			continue // ended since the list was taken
		case st == windows.STATUS_ACCESS_DENIED:
			cause = threadHidden
		case st != 0:
			cause = "opening it: " + st.Error()
		default:
			cause = checkThread(h, set, wantACL, mismatch)
			windows.CloseHandle(h)
		}
		if cause != "" {
			if !slices.Contains(causes, cause) {
				causes = append(causes, cause)
			}
			ids = append(ids, strconv.FormatUint(uint64(id), 10))
		}
	}
	if len(causes) > 0 {
		// The cause is the reason; which threads differ at every start and
		// are the detail.
		slices.Sort(causes)
		return &failure{reason: strings.Join(causes, "; "), detail: "thread ids " + strings.Join(ids, ", ")}
	}
	return nil
}

// checkThread gives h's thread set where it may, reads its list back and
// says what is wrong with it ("" when it is the restricted one).
func checkThread(h windows.Handle, set *windows.SECURITY_DESCRIPTOR, wantACL *windows.ACL, mismatch string) string {
	g, err := grantedAccess(h)
	if err == nil && set != nil && g&windows.WRITE_DAC != 0 {
		err = windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, set)
	}
	switch {
	case err != nil:
		return err.Error()
	case g&windows.READ_CONTROL == 0:
		return threadUnreadable
	}
	got, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	var acl *windows.ACL
	if err == nil {
		acl, _, err = got.DACL()
	}
	switch {
	case err != nil:
		return threadUnreadable
	case !sameACL(acl, wantACL):
		return mismatch
	}
	return ""
}

// sameACL: a and b hold the same entries in the same order.
func sameACL(a, b *windows.ACL) bool {
	if a == nil || b == nil || a.AceCount != b.AceCount {
		return false
	}
	for i := uint32(0); i < uint32(a.AceCount); i++ {
		var x, y *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(a, i, &x) != nil || windows.GetAce(b, i, &y) != nil {
			return false
		}
		if x.Header != y.Header || x.Mask != y.Mask ||
			!(*windows.SID)(unsafe.Pointer(&x.SidStart)).Equals((*windows.SID)(unsafe.Pointer(&y.SidStart))) {
			return false
		}
	}
	return true
}

// ownToken is this process's token, opened once, before its access list is
// closed: the handle keeps the rights it was opened with.
var ownToken windows.Token

const leavesThreads = "another program running as the same user can open one of Tenant's threads and redirect it (SetThreadContext, QueueUserAPC), which runs code inside Tenant and so reads its memory"

func protect() []Control {
	if len(os.Args) == 2 && os.Args[1] == anchorArg {
		anchor() // the helper that keeps a console open: it does nothing else, and exits
	}
	sd, err := windows.SecurityDescriptorFromString(processSDDL)
	var threadSD *windows.SECURITY_DESCRIPTOR
	if err == nil {
		threadSD, err = windows.SecurityDescriptorFromString(threadSDDL)
	}
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
		Name:   "reading or redirecting a thread refused (every thread's access list, and the default for new ones); the owner can still end, wait for and resume a thread, and read its access list",
		Leaves: leavesThreads,
	}, set(func() error { return restrictThreads(threadSD) }))
	process := apply(Control{ID: "windows.process-dacl",
		Name:   "reading memory, writing a dump and attaching a debugger refused (the process's access list)",
		Leaves: "another program running as the same user can read Tenant's memory (ReadProcessMemory), write a dump of it (Task Manager's \"Create dump file\"), attach a debugger and start a thread in it",
	}, set(func() error {
		return windows.SetKernelObjectSecurity(windows.CurrentProcess(), windows.DACL_SECURITY_INFORMATION, sd)
	}))
	// The console host opens the process again only once its list is closed.
	var note string
	console := apply(Control{ID: "windows.console-host",
		Name:   "the console host's handle to this process carries the rights the owner keeps and no more (the console is left and joined again once the access list is set)",
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

	// Every thread's list is established, never inferred (decision A80): a
	// program in the moment before Protect can give a thread a list that
	// refuses its owner WRITE_DAC and lets everyone in, one that grants this
	// process nothing, or one with an entry for a SID this process does not
	// hold (another logon session of the user). checkThreads sets, reads back
	// and compares every thread the system lists.
	err = checkThreads(sd, maxThreads)
	var f *failure
	if errors.As(err, &f) && f.reason != threadsTooMany {
		f.reason = "a thread that existed at start could not be given the access list: " + f.reason
	}
	return err
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
//     held to restrictedSDDL's terms, and every thread's own list is read
//     again (the owner keeps READ_CONTROL on threads) and held to
//     threadReadBack: a list changed after the start through a handle taken
//     before Protect is no longer reported as in force.
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
		if dacl, _, err := sd.DACL(); err != nil || !closedToTheUser(dacl, ownerProcessRights) {
			found = "the process's access list gives the user's other programs more than the rights the owner keeps"
		}
		return found, true
	case "windows.thread-dacl":
		if dacl, err := tokenDefaultDACLOf(ownToken); err == nil && !closedToTheUser(dacl, ownerThreadRights) {
			return "the access list a new thread gets gives the user's other programs more than the rights the owner keeps", true
		}
		var f *failure
		switch err := checkThreads(nil, 0); {
		case err == nil:
			return "", true
		case errors.As(err, &f):
			return "a thread: " + f.reason, true
		}
		return "", false // the threads could not be listed
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
// likes, the owner no more than owner, and nobody else anything.
func closedToTheUser(dacl *windows.ACL, ownerMask uint32) bool {
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
		case sid.Equals(owner) && uint32(ace.Mask)&^ownerMask == 0:
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
