//go:build windows

package main

import "golang.org/x/sys/windows/svc/eventlog"

// systemLogSource is the event source Tenant writes as. A variable for the
// live test, which writes as a source of its own.
var systemLogSource = "Tenant"

// openSystemLog registers the source for this process. Any account may; it
// writes nothing.
func openSystemLog() (*eventlog.Log, error) { return eventlog.Open(systemLogSource) }

// writeSystemLog writes msg as an error in the Application event log. The
// source has no message file registered (that takes an administrator and a
// registry key), so Event Viewer shows the text under its "the description
// cannot be found" preamble.
func writeSystemLog(msg string) error {
	l, err := openSystemLog()
	if err != nil {
		return err
	}
	defer l.Close()
	return l.Error(1, msg)
}
