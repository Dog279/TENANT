//go:build windows

package main

// svc_windows.go (TEN-498): the Windows service entry. `tenant service
// install --system` registers tenant.exe with the SCM, which starts it with
// no console and in no user's session, so neither a console event nor the
// window messages a logon task gets at shutdown (sessionend_windows.go)
// ever come; Stop, Shutdown and PreShutdown arrive here instead and end
// serve the way SIGTERM does. PreShutdown is accepted because it carries the
// longer grace (180 s by default) a teardown mid-turn needs (PR E).

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"

	"tenant/internal/procprotect"
)

// runWindowsService runs serve under the SCM when that is who started this
// process; false means an ordinary command line.
func runWindowsService() bool {
	if ok, _ := svc.IsWindowsService(); !ok {
		return false
	}
	// ponytail: the wait hint is the budget the unit passes (--stop-budget
	// 120s, specFor); parse serve's flags here if someone edits binPath.
	h := &tenantService{budget: 120 * time.Second, run: func(ctx context.Context) error {
		if len(os.Args) < 2 || (os.Args[1] != "serve" && os.Args[1] != "child") {
			return errors.New("the service's command line must be `tenant serve …` or `tenant child run …`")
		}
		holdDataDir = lockDataDirForProcess
		runningCommand = "serve"
		killChildrenOnExit()
		var err error
		procprotect.Ordinary(func() { // as main runs a command
			if os.Args[1] == "child" { // a fleet child installed with --system (TEN-531)
				err = cmdChild(ctx, os.Args[2:])
				return
			}
			err = cmdServe(ctx, os.Args[2:])
		})
		return err
	}}
	if err := svc.Run("Tenant", h); err != nil {
		os.Exit(1)
	}
	return true
}

// tenantService is the SCM handler: serve runs in a goroutine; a stop
// control cancels its context, reports StopPending with the budget as the
// wait hint, and reports Stopped once serve returns (or the budget plus a
// margin has passed, which leaves the SCM to kill what is left).
type tenantService struct {
	run    func(ctx context.Context) error
	budget time.Duration
}

func (t *tenantService) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- t.run(ctx) }()
	cur := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown}
	s <- cur
	for {
		select {
		case err := <-done: // serve ended on its own
			s <- svc.Status{State: svc.Stopped}
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- cur
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				cur = svc.Status{State: svc.StopPending, WaitHint: uint32(t.budget / time.Millisecond)}
				s <- cur
				cancel()
				select {
				case <-done:
				case <-time.After(t.budget + 5*time.Second):
				}
				s <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}
