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
	// Reason is why it is not in force.
	Reason string `json:"reason,omitempty"`
	// Leaves is what its absence leaves open.
	Leaves string `json:"leaves"`
	// Note qualifies a control that is in force ("attach denied, not verified").
	Note string `json:"note,omitempty"`
}

// Report is what Protect did.
type Report struct {
	// Disabled: the build has the controls compiled out (-tags debugattach).
	Disabled bool `json:"disabled"`
	// Controls are this system's controls, in the order they were applied.
	// Empty on a system that has none, and when Disabled.
	Controls []Control `json:"controls"`
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
	return Report{Controls: protect()}
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
		return c
	}
	c.OK = true
	return c
}

// stillOpen is what the controls leave readable on each system.
var stillOpen = map[string]string{
	"linux":   "the command line and basic status stay readable to the user's other programs; swap and hibernation are the disk's business",
	"darwin":  "the command line and the environment the process started with stay readable to the user's other programs",
	"windows": "the command line, the program's path and its account details stay readable to the user's other programs; the page file and hibernation are the disk's business",
}

// StillOpen says, in plain words, what the controls leave readable on goos
// (runtime.GOOS); "" for a system that has no controls.
func StillOpen(goos string) string { return stillOpen[goos] }

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
