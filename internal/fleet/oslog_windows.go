//go:build windows

package fleet

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/svc/eventlog"
)

// osLogSource is the child's event source in the Windows Application log.
const osLogSource = "TenantChild"

// OpenOSLog opens the operating system's log for a child's audit lines: the
// Windows Event Log (Application, source TenantChild). It returns what
// writes one line and what closes the log.
func OpenOSLog() (write func(line string) error, done func(), err error) {
	l, err := eventlog.Open(osLogSource)
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: the event log: %w", err)
	}
	return func(line string) error { return l.Info(1, line) }, func() { _ = l.Close() }, nil
}

// RegisterOSLog names the child's event source to Windows, so Event Viewer
// shows its lines as they are instead of under "the description cannot be
// found". It writes under HKLM and so needs an administrator; a child whose
// source is not registered still logs.
func RegisterOSLog() error {
	err := eventlog.InstallAsEventCreate(osLogSource, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("fleet: register the event source: %w", err)
	}
	return nil
}

// UnregisterOSLog removes what RegisterOSLog wrote.
func UnregisterOSLog() error { return eventlog.Remove(osLogSource) }
