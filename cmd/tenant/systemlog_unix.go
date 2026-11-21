//go:build !windows

package main

import "log/syslog"

// systemLogSource is the syslog tag Tenant writes as. A variable for the
// live test, which writes under a tag of its own.
var systemLogSource = "tenant"

// openSystemLog connects to the system's syslog socket. It writes nothing.
func openSystemLog() (*syslog.Writer, error) {
	return syslog.New(syslog.LOG_ERR|syslog.LOG_DAEMON, systemLogSource)
}

// writeSystemLog writes msg to syslog as a daemon error: the journal under
// systemd (journalctl -t tenant), the system log elsewhere.
func writeSystemLog(msg string) error {
	w, err := openSystemLog()
	if err != nil {
		return err
	}
	defer w.Close()
	return w.Err(msg)
}
