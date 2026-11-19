package procprotect

import (
	"context"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// macOS. PT_DENY_ATTACH tells the kernel to refuse a later ptrace attach to
// this process. The soft core limit of 0 stops a core file.
//
// PT_DENY_ATTACH is for a binary that was not signed with the hardened
// runtime, such as one built from source. What it stops on its own has not
// been measured on a Mac (the spec measured the signed binary, which refuses
// the debugger outright), so the control is reported as "attach denied, not
// verified". Signing the installed binary is the install script's, `tenant
// service install`'s and `tenant update`'s job, and is not done here.
//
// If a debugger is already attached when Protect runs, the kernel ends the
// process: debug a build made with -tags debugattach.
//
// Still readable, whatever is done here: the command line, and the
// environment the process started with. That is why a secret must not arrive
// in an environment variable on macOS (cmd/tenant/secretenv.go).
func protect() []Control {
	return []Control{
		apply(Control{ID: "darwin.deny-attach",
			Name:   "debugger attach refused (PT_DENY_ATTACH)",
			Note:   "attach denied, not verified",
			Leaves: "a debugger run by the same user can attach to Tenant and read its memory once macOS's own check lets it (by default it asks for an administrator's password)",
		}, unix.PtraceDenyAttach),
		apply(Control{ID: "darwin.core-limit",
			Name:   "no core file (soft core limit 0)",
			Leaves: "a crash can write Tenant's memory to a core file, where core files are turned on",
		}, noCoreFile),
	}
}

// reread reads a control's state again, for Now. The core limit can be read;
// whether the kernel still refuses an attach cannot be asked.
func reread(id string) (found string, whole bool) {
	if id == "darwin.core-limit" {
		return coreLimitNow()
	}
	return "", false
}

// What system reports is the machine's, the same whichever process reads it.
const systemIsOfTheProcess = false

// system is what macOS says about itself: whether System Integrity
// Protection is on (with it off, any program of the user can attach to any
// other), and whether the disk is encrypted (swap and the sleep image hold
// memory).
func system() []string {
	say := func(what, tool string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tool, "status").Output()
		return statusLine(what, out, err)
	}
	return []string{say("System Integrity Protection", "/usr/bin/csrutil"), say("FileVault", "/usr/bin/fdesetup")}
}
