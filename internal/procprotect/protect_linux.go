package procprotect

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux. With the dumpable flag off the kernel refuses ptrace,
// /proc/<pid>/mem, process_vm_readv, pidfd_getfd and perf_event_open from any
// process without CAP_SYS_PTRACE, makes the process's /proc entries root's,
// so /proc/<pid>/environ is unreadable too, and writes no crash dump: a
// process that is not dumpable is skipped before the kernel looks at
// kernel.core_pattern, so a crash collector it pipes to gets nothing. The
// soft core limit of 0 is a second guard for dumps to a file.
//
// The kernel sets the flag again when a program is replaced by another
// (execve), so a command Tenant starts is an ordinary process; it inherits
// the soft limit and may raise it.
//
// Still readable: the command line and /proc/<pid>/status.
func protect() []Control {
	return []Control{
		apply(Control{ID: "linux.dumpable",
			Name:   "memory reads, debugger attach and crash dumps refused (the dumpable flag is off)",
			Leaves: "another program running as the same user can read Tenant's memory and its environment (ptrace, /proc/<pid>/mem, process_vm_readv, /proc/<pid>/environ) wherever kernel.yama.ptrace_scope lets it, and a crash can leave a memory dump",
		}, func() error {
			if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
				return err
			}
			if v, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || v != 0 {
				return errors.Join(errors.New("the dumpable flag did not stay off"), err)
			}
			return nil
		}),
		apply(Control{ID: "linux.core-limit",
			Name:   "no core file (soft core limit 0)",
			Leaves: "nothing while the dumpable flag is off; without it a crash can write Tenant's memory to a core file",
		}, noCoreFile),
	}
}

// reread reads a control's state again, for Now: found says what is wrong
// with it, and whole that everything the control covers was read. Both of
// Linux's can be read.
func reread(id string) (found string, whole bool) {
	switch id {
	case "linux.dumpable":
		v, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		if err == nil && v != 0 {
			found = "the dumpable flag is on"
		}
		return found, err == nil
	case "linux.core-limit":
		return coreLimitNow()
	}
	return "", false
}

// What system reports is the machine's, the same whichever process reads it.
const systemIsOfTheProcess = false

// system is the kernel's own ptrace restriction, shown beside the flag. It
// is not what protects Tenant: at 0 it restricts nothing, and at 1 it still
// lets a process's ancestors in.
func system() []string {
	b, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil {
		return []string{"kernel.yama.ptrace_scope: not present (this kernel has no Yama)"}
	}
	return []string{"kernel.yama.ptrace_scope=" + strings.TrimSpace(string(b)) + " (the system's own setting, not what protects Tenant)"}
}
