//go:build !windows

package fleet

import (
	"fmt"
	"log/syslog"
)

// OpenOSLog opens the operating system's log for a child's audit lines:
// syslog, which on a systemd machine is the journal. It returns what writes
// one line and what closes the log. A machine with no syslog to talk to gets
// an error, and its child runs with the audit file alone.
func OpenOSLog() (write func(line string) error, done func(), err error) {
	// The facility of sshd and sudo: these lines say what was done to the
	// machine from elsewhere.
	w, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_NOTICE, "tenant-child")
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: the system log: %w", err)
	}
	return w.Notice, func() { _ = w.Close() }, nil
}

// RegisterOSLog and UnregisterOSLog are Windows' (an event source); syslog
// needs neither.
func RegisterOSLog() error   { return nil }
func UnregisterOSLog() error { return nil }
