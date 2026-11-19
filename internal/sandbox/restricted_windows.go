//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"tenant/internal/procprotect"
)

// Windows has no restricted backend: the confining sandbox on every OS is the
// planned container backend (docs/plans/docker-sandbox-plan.md). The Job Object
// backend below is a resource-limit primitive, resolved as "jobobject": it
// enforces job memory / CPU-time / active-process caps and kill-on-close, and
// confines no filesystem root or network.

// Compile-time proof that the pure JOB_OBJECT_LIMIT_* values in joblimits.go still
// equal golang.org/x/sys/windows' own. If x/sys ever renumbered one, the XOR is
// non-zero and uint32(0) minus a positive constant is a negative constant, which
// overflows uint32 and stops this file compiling — a loud, early failure instead
// of a silently wrong limit mask.
const (
	_ = uint32(0) - (jobLimitProcessTime ^ uint32(windows.JOB_OBJECT_LIMIT_PROCESS_TIME))
	_ = uint32(0) - (jobLimitActiveProcess ^ uint32(windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS))
	_ = uint32(0) - (jobLimitJobMemory ^ uint32(windows.JOB_OBJECT_LIMIT_JOB_MEMORY))
	_ = uint32(0) - (jobLimitKillOnClose ^ uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE))
)

// windowsRestricted runs a command in a Windows Job Object: resource limits and
// kill-on-close, nothing more. Probe says so, so nothing is over-claimed.
type windowsRestricted struct{}

// newJobObjectBackend returns the Windows Job Object limits primitive.
func newJobObjectBackend() Backend { return &windowsRestricted{} }

func (*windowsRestricted) Name() string         { return "jobobject" }
func (*windowsRestricted) Isolation() Isolation { return IsolationProcess }

func (*windowsRestricted) Probe(context.Context) Capabilities {
	return Capabilities{
		Backend:    "jobobject",
		Isolation:  IsolationProcess,
		Filesystem: EnforceNone, // a Job Object confines no filesystem root
		Network:    EnforceNone, // nor the network
		Limits:     EnforceFull, // Job Object: job memory, active-process, CPU-time, kill-on-close
		Env:        EnforceFull, // the env allowlist is honoured via materialize
		Shell:      "powershell",
		Detail: "windows Job Object limits primitive: " +
			"job memory, active-process, CPU-time and kill-on-close are enforced; no filesystem-root or " +
			"network enforcement",
	}
}

func (b *windowsRestricted) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	job, err := newLimitedJob(pol.Limits)
	if err != nil {
		return Result{}, err
	}
	defer windows.CloseHandle(job) // KILL_ON_JOB_CLOSE reaps any straggler on close

	// --- Launch the child SUSPENDED so we can assign it to the job before it
	// executes a single instruction, then resume its main thread. ---
	name, args := shellArgs(cmd)
	c := exec.CommandContext(ctx, name, args...)
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	if pol.WorkDir != "" {
		c.Dir = pol.WorkDir
	}
	c.Env = pol.Env.materialize(os.Environ())
	// Output is bounded as it arrives (TEN-438): clip's head+tail shape, never
	// the whole stream in memory.
	buf := &headTailBuffer{max: pol.Limits.MaxOutputBytes}
	c.Stdout = buf
	c.Stderr = buf

	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: uint32(windows.CREATE_SUSPENDED)}

	if err := procprotect.Start(c); err != nil {
		return Result{}, fmt.Errorf("sandbox: start child: %w", err)
	}

	if err := assignAndResume(job, c.Process.Pid); err != nil {
		_ = c.Process.Kill() // never leak a suspended child
		return Result{}, fmt.Errorf("sandbox: assign+resume: %w", err)
	}

	werr := c.Wait()
	res := Result{Output: buf.String(), PID: pidOf(c)}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		res.ExitCode = ee.ExitCode()
		res.ExitStatus = ee.Error()
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		return res, nil
	}
	if werr != nil {
		if ee != nil {
			return res, nil // a non-zero exit is signal, not an infra error
		}
		return res, werr
	}
	return res, nil
}

// newLimitedJob creates a Job Object carrying the policy's limits and
// kill-on-close.
func newLimitedJob(l Limits) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("sandbox: CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	jl := jobLimitsFrom(l)
	info.BasicLimitInformation.LimitFlags = jl.Flags
	info.BasicLimitInformation.ActiveProcessLimit = jl.ActiveProcessLimit
	info.BasicLimitInformation.PerProcessUserTimeLimit = jl.PerProcessUserTime100ns
	info.JobMemoryLimit = uintptr(jl.JobMemoryLimit)
	if _, err := windows.SetInformationJobObject(
		job,
		uint32(windows.JobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("sandbox: SetInformationJobObject: %w", err)
	}
	return job, nil
}

// Start runs argv as a Windows program inside a Job Object: its memory, CPU
// time and process count are capped, with kill-on-close, and its environment
// is the policy's allowlist. Like Run, it confines no filesystem root or
// network. The child starts suspended and is assigned to the job before it
// runs.
func (b *windowsRestricted) Start(ctx context.Context, argv []string, pol Policy) (*Process, error) {
	job, err := newLimitedJob(pol.Limits)
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if pol.WorkDir != "" {
		c.Dir = pol.WorkDir
	}
	c.Env = pol.Env.materialize(os.Environ())
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: uint32(windows.CREATE_SUSPENDED)}
	return startProcess(c, pol.Stderr, func(pid int) error { return assignAndResume(job, pid) },
		func() { windows.CloseHandle(job) }) // kill-on-close ends anything the server left behind
}

func init() { registerJobObject(newJobObjectBackend) }

// assignAndResume attaches the suspended child (pid) to the job, then resumes
// its main thread. The child was created CREATE_SUSPENDED, so it has run no code
// and cannot escape the job before assignment.
func assignAndResume(job windows.Handle, pid int) error {
	hProc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(hProc)
	if err := windows.AssignProcessToJobObject(job, hProc); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return resumeMainThread(uint32(pid))
}

// resumeMainThread resumes the single main thread of a process created
// suspended. It walks a thread snapshot to find the thread because os/exec does
// not surface the child's thread handle.
func resumeMainThread(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	for err := windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			return fmt.Errorf("OpenThread: %w", oerr)
		}
		_, rerr := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if rerr != nil {
			return fmt.Errorf("ResumeThread: %w", rerr)
		}
		return nil // a suspended child has exactly one thread — the main thread
	}
	return fmt.Errorf("sandbox: main thread for pid %d not found in snapshot", pid)
}
