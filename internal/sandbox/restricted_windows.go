//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The registered Windows restricted backend is the WSL-bubblewrap backend in
// wsl_windows.go (all restricted → WSL). The Job Object backend below is kept as
// a resource-limit primitive: it enforces job memory / CPU-time / active-process
// caps and kill-on-close, which bubblewrap-through-WSL does not, and it is the
// foundation the native-Windows confinement path (TEN-346) will build on. It is
// exercised directly by tests, not resolved by `restricted`.

// errNotImplemented marks a Windows confinement primitive that is scaffolded but
// not yet built out. Run tolerates it (the Job Object limits still apply) and
// Probe reports the matching dimension as un-/partly-enforced, so the security
// audit never overstates the boundary. A Windows engineer completes these under
// TEN-346.
var errNotImplemented = errors.New("sandbox: windows restricted primitive not implemented (TODO TEN-346)")

// Compile-time proof that the pure JOB_OBJECT_LIMIT_* values in spec.go still
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

// windowsRestricted confines a command with a Windows Job Object (resource
// limits + kill-on-close, live today) and — once the TODO(TEN-346) scaffolding
// below is completed — a low-privilege dedicated-account token, NTFS ACEs on the
// work/read/write roots, and a WFP egress filter. Probe reports exactly which of
// those are live so nothing is over-claimed.
type windowsRestricted struct{}

// newJobObjectBackend returns the Windows Job Object limits primitive. It is NOT
// the registered restricted backend (that is WSL bubblewrap, wsl_windows.go);
// tests construct it directly to validate the resource-limit enforcement, and
// the native-Windows confinement path (TEN-346) will build on it.
func newJobObjectBackend() Backend { return &windowsRestricted{} }

func (*windowsRestricted) Name() string         { return "jobobject" }
func (*windowsRestricted) Isolation() Isolation { return IsolationProcess }

func (*windowsRestricted) Probe(context.Context) Capabilities {
	return Capabilities{
		Backend:    "jobobject",
		Isolation:  IsolationProcess,
		Filesystem: EnforceNone, // no root is enforced until the NTFS ACEs land (TODO TEN-346)
		Network:    EnforceNone, // WFP ALE_AUTH_CONNECT egress filter is TODO
		Limits:     EnforceFull, // Job Object: job memory, active-process, CPU-time, kill-on-close
		Env:        EnforceFull, // the env allowlist is honoured via materialize
		Shell:      "powershell",
		Detail: "windows Job Object limits primitive (not the restricted backend; restricted uses WSL): " +
			"job memory, active-process, CPU-time and kill-on-close are enforced; no filesystem-root or " +
			"network enforcement (native token/ACE/WFP work is TODO TEN-346)",
	}
}

func (b *windowsRestricted) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	// --- Pending confinement primitives (scaffolded). Each is invoked on the
	// path a completed implementation uses; errNotImplemented is tolerated so the
	// live Job Object still applies. A real error from a finished primitive fails
	// the run closed. ---
	token, err := b.dedicatedAccountToken()
	if err != nil && !errors.Is(err, errNotImplemented) {
		return Result{}, fmt.Errorf("sandbox: dedicated account token: %w", err)
	}
	if err := applyRootACEs(pol, token); err != nil && !errors.Is(err, errNotImplemented) {
		return Result{}, fmt.Errorf("sandbox: root ACEs: %w", err)
	}
	if pol.Network == NetNone || pol.Network == NetAllowlist {
		cleanup, err := installEgressFilter(pol)
		if err != nil && !errors.Is(err, errNotImplemented) {
			return Result{}, fmt.Errorf("sandbox: egress filter: %w", err)
		}
		if cleanup != nil {
			defer cleanup()
		}
	}

	// --- Job Object: the live confinement. ---
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
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf

	spa := &syscall.SysProcAttr{CreationFlags: uint32(windows.CREATE_SUSPENDED)}
	if token != 0 {
		// When the dedicated-account work lands, run the child in that token's
		// security context instead of Tenant's own.
		spa.Token = syscall.Token(uintptr(token))
	}
	c.SysProcAttr = spa

	if err := c.Start(); err != nil {
		return Result{}, fmt.Errorf("sandbox: start child: %w", err)
	}

	if err := assignAndResume(job, c.Process.Pid); err != nil {
		_ = c.Process.Kill() // never leak a suspended child
		return Result{}, fmt.Errorf("sandbox: assign+resume: %w", err)
	}

	werr := c.Wait()
	res := Result{Output: clip(buf.String(), pol.Limits.MaxOutputBytes), PID: pidOf(c)}
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
// network (TEN-346). The child starts suspended and is assigned to the job
// before it runs.
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

// --- Scaffolded confinement primitives (TODO TEN-346). Each compiles, is
// invoked by Run on the correct path, and returns errNotImplemented so a Windows
// engineer can complete and test it without re-plumbing the call sites. This is
// the operator-approved design (decision 2026-09-22): a dedicated low-privilege
// sandbox account, NTFS ACEs on the roots, and a WFP egress filter. ---

// dedicatedAccountToken logs on / derives the low-privilege dedicated sandbox
// account and returns a restricted primary token to launch the child with. A
// zero token means "not available" and Run then launches in Tenant's own context.
//
// TODO(TEN-346): LogonUser(dedicated sandbox account) then CreateRestrictedToken
// to strip privileges and add restricting SIDs.
func (*windowsRestricted) dedicatedAccountToken() (windows.Token, error) {
	return 0, errNotImplemented
}

// applyRootACEs stamps NTFS DACLs for the token's SID so the filesystem policy
// is enforced by the OS: grant on WorkDir/WriteRoots (modify) and ReadRoots
// (read), deny on DenyRoots.
//
// TODO(TEN-346): build an EXPLICIT_ACCESS list from pol and apply it with
// SetNamedSecurityInfo on each root.
func applyRootACEs(pol Policy, token windows.Token) error {
	_ = pol
	_ = token
	return errNotImplemented
}

// installEgressFilter adds a Windows Filtering Platform ALE_AUTH_CONNECT filter
// scoped to the child so outbound connections are blocked (NetNone) or limited
// to pol.Allow (NetAllowlist). The returned cleanup removes the filter.
//
// TODO(TEN-346): add a WFP sublayer plus FWPM_LAYER_ALE_AUTH_CONNECT_V4/V6
// filters keyed on the dedicated account's SID; return a cleanup that tears them
// down.
func installEgressFilter(pol Policy) (cleanup func(), err error) {
	_ = pol
	return nil, errNotImplemented
}
