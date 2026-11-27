package main

import (
	"fmt"
	"strings"
)

// What an update that restarts nothing leaves behind (TEN-556). `tenant
// update` restarts the unit `tenant service` installed. A Tenant started any
// other way (a scheduled task or a unit of the operator's own, a terminal)
// goes on running the old version, and "restart Tenant to finish the update"
// did not say what that was: on the R340 it was two scheduled tasks. The
// update now names the processes that run the old binary and, on Windows,
// the scheduled tasks that start it.
//
// ponytail: it names them and restarts nothing. Ending a task is a hard kill,
// and whether a task of the operator's own may be ended is the operator's
// call; restart them here once `tenant service` can adopt such a task.

// runningTenant is the other processes that run the binary at exe: "pid N",
// with the command line where the system shows it (processesOf, per system).
// It is read before the swap, which gives the file those processes run
// another name. Tests swap it.
var runningTenant = processesOf

// startersOf is what starts the binary at exe besides `tenant service`: the
// scheduled tasks whose action runs it, on Windows (tasksOf). Tests swap it.
var startersOf = tasksOf

// restartNotice is what `tenant update` says once the binary is swapped and
// it restarted nothing: old is the version that was replaced.
func restartNotice(old, new string, running, tasks []string) string {
	var b strings.Builder
	if len(running) == 0 {
		fmt.Fprintf(&b, "updated %s -> %s; no process this account can see runs %s, so the new version is what starts next\n", old, new, old)
	} else {
		fmt.Fprintf(&b, "updated %s -> %s; restart Tenant to finish the update. Still running %s:\n", old, new, old)
		for _, p := range running {
			b.WriteString("  " + p + "\n")
		}
	}
	if len(tasks) > 0 {
		b.WriteString("Scheduled tasks that start this binary (`tenant service` installed none of them, so it restarts none):\n")
		for _, t := range tasks {
			b.WriteString("  " + t + "\n")
		}
		b.WriteString("Restart each one: schtasks /End /TN \"<task>\", then schtasks /Run /TN \"<task>\"\n")
	}
	return b.String()
}
