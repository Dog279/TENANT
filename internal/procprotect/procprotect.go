// Package procprotect asks the operating system to keep other programs that
// run as the same user out of this process (TEN-535): no reading its memory,
// no debugger, no crash dump. main calls Protect before anything else, for
// every command.
//
// Every control is the system refusing an ordinary user's request. None stops
// an administrator or root, a program that started Tenant itself, or a reader
// of the plain files on disk. What each one stops, and what stays readable on
// each system, is in protect_<os>.go and StillOpen.
//
// There is no switch at run time: a same-user program could restart Tenant
// with it set. A developer who needs a debugger builds with -tags debugattach,
// which compiles the controls out (Disabled).
package procprotect

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
)

// Control is one protection Protect applies on this system.
type Control struct {
	// ID is stable ("linux.dumpable"): an acknowledgement of a missing
	// control is recorded against it.
	ID string `json:"id"`
	// Name says what the control does, in plain words.
	Name string `json:"name"`
	// OK is true when the control is in force.
	OK bool `json:"ok"`
	// Reason is why it is not in force. It is the same at every start for
	// the same cause: an acknowledgement of a missing control is matched on
	// it. What varies from start to start (an id, a count) is in Detail.
	Reason string `json:"reason,omitempty"`
	// Detail is what Reason leaves out. Nothing matches on it.
	Detail string `json:"detail,omitempty"`
	// Leaves is what its absence leaves open.
	Leaves string `json:"leaves"`
	// Note qualifies a control that is in force ("attach denied, not verified").
	Note string `json:"note,omitempty"`
	// Undone: the control was in force when the process started and is not
	// any more. Reason says what was found. Only Now sets it.
	Undone bool `json:"undone,omitempty"`
	// AsAtStart: the system does not let the process read this control's
	// state again, or not all of it, so OK is what was true when the process
	// started. Only Now sets it.
	AsAtStart bool `json:"as_at_start,omitempty"`
}

// Report is what Protect did.
type Report struct {
	// Disabled: the build has the controls compiled out (-tags debugattach).
	Disabled bool `json:"disabled"`
	// Controls are this system's controls, in the order they were applied.
	// Empty on a system that has none, and when Disabled.
	Controls []Control `json:"controls"`
	// PID is the process the report is of. Only Now sets it.
	PID int `json:"pid,omitempty"`
	// System is what the operating system says about that process, where
	// that is a fact about the process and not about the machine. Only Now
	// sets it.
	System []string `json:"system,omitempty"`
}

// Missing returns the controls that are not in force.
func (r Report) Missing() []Control {
	var out []Control
	for _, c := range r.Controls {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Protect applies this system's controls to the running process and reports
// each one. Call it first in main. It never fails the start: what to do about
// a missing control is the caller's decision.
func Protect() Report {
	if Disabled {
		return Report{Disabled: true}
	}
	r := Report{Controls: protect()}
	appliedMu.Lock()
	applied = r
	appliedMu.Unlock()
	return r
}

var (
	appliedMu sync.Mutex
	applied   Report // what Protect last reported in this process
)

// ExclusionPutBack is the note on windows.wer-exclusion when this start
// found the program's name missing from the user's exclusion list and put it
// there: on the first start of a program of that name, or after something
// took it off. Every command applies the controls at its start, so a
// `tenant doctor` puts back the exclusion of a running Tenant of the same
// file name before it asks that Tenant (checkProcessProtection says so).
const ExclusionPutBack = "this start put the program on the list: it was not on it"

// undone starts the reason of a control that was in force and is not.
const undone = "it was in force when Tenant started and is not any more: "

// Now is this process's report as it stands: what Protect reported at start,
// with each control read again where the system lets the process read it.
// Another program of the same user can undo some controls from outside (the
// Windows Error Reporting exclusion is a registry value of the user's, and
// the soft core limit can be raised with prlimit); the report of the start
// would go on saying they are in force.
//
// A control that was in force and is not any more comes back not OK and
// Undone, with what was found as its reason. One that cannot be read again,
// or not wholly, is marked AsAtStart. A control that was not in force at
// start is left as it was reported.
//
// A running serve answers doctor with it (GET /api/status).
func Now() Report {
	appliedMu.Lock()
	r := Report{Disabled: Disabled, Controls: slices.Clone(applied.Controls), PID: os.Getpid()}
	appliedMu.Unlock()
	for i := range r.Controls {
		c := &r.Controls[i]
		if !c.OK {
			continue
		}
		switch found, whole := reread(c.ID); {
		case found != "":
			c.OK, c.Undone, c.Reason = false, true, undone+found
		case !whole:
			c.AsAtStart = true
		}
	}
	if systemIsOfTheProcess {
		r.System = system()
	}
	return r
}

var (
	forcedMu sync.Mutex
	forced   = map[string]string{}
)

// ForceFail is for tests: until it is called again with an empty reason,
// Protect does not apply the control named id and reports it as not in force,
// for reason. It is how a test gets the report of a system where a control
// cannot be applied. A test that only needs such a report, without running
// Protect in its own process, can build the Report itself.
func ForceFail(id, reason string) {
	forcedMu.Lock()
	defer forcedMu.Unlock()
	if reason == "" {
		delete(forced, id)
		return
	}
	forced[id] = reason
}

// apply runs do, the control's system calls, and files the outcome in c.
func apply(c Control, do func() error) Control {
	forcedMu.Lock()
	reason, skip := forced[c.ID]
	forcedMu.Unlock()
	if skip {
		c.Reason = reason
		return c
	}
	if err := do(); err != nil {
		c.Reason = err.Error()
		var f *failure
		if errors.As(err, &f) {
			c.Detail = f.detail
		}
		return c
	}
	c.OK = true
	return c
}

// failure is a control's error whose text is its reason, the same at every
// start, with what varies kept beside it.
type failure struct{ reason, detail string }

func (f *failure) Error() string { return f.reason }

// stillOpen is what the controls leave readable on each system.
var stillOpen = map[string]string{
	"linux":   "the command line and basic status stay readable to the user's other programs; swap and hibernation are the disk's business",
	"darwin":  "the command line and the environment the process started with stay readable to the user's other programs",
	"windows": "the command line, the program's path and its account details stay readable to the user's other programs; the page file and hibernation are the disk's business",
}

// StillOpen says, in plain words, what the controls leave readable on goos
// (runtime.GOOS); "" for a system that has no controls.
func StillOpen(goos string) string { return stillOpen[goos] }

// notStopped is what a program of the same user can still do to a protected
// Tenant on each system, beyond reading what stillOpen lists.
var notStopped = map[string][]string{
	"windows": {
		"a program of the same user can end Tenant, and can end, wait for and resume each of its threads; ending threads stops work and reads nothing",
		"whoever started Tenant holds a handle to it from before these controls, with full access, for as long as it keeps it (a shell waiting for the command, for one), and a program of the same user can copy that handle out of the starter",
		"the controls refuse a program that starts after Tenant has finished starting; a program already running when Tenant starts (so also one that ends Tenant and waits for it to be started again) can still get in, through the instant before the controls or through the full-access handle the console host holds until Tenant has joined its console again, and what it takes then keeps working; closing that takes Tenant being started already closed, which this version does not do",
		"a program of the same user can make the Windows Error Reporting exclusion fail at start (delete the value and deny writes to the key), so that Tenant asks at a terminal whether to go on without it or, as a hidden serve, refuses to start",
	},
}

// NotStopped says, a line each, what the user's other programs can still do
// to a protected Tenant on goos (runtime.GOOS) that is particular to that
// system. The limits every system shares are the caller's to state.
func NotStopped(goos string) []string { return notStopped[goos] }

// System is what the operating system itself reports about this process's
// exposure, where that can be read without elevation: a line per fact.
func System() []string { return system() }

// statusLine is the first line one of the system's own status tools printed
// (macOS: csrutil status, fdesetup status), or that it could not be asked.
func statusLine(what string, out []byte, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line = strings.TrimSpace(line)
	if err != nil || line == "" {
		return what + ": could not be read"
	}
	if !strings.Contains(line, what) {
		line = what + ": " + line
	}
	return line
}
