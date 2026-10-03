//go:build windows

package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// Stop, Shutdown and PreShutdown each cancel serve once and walk the
// status through Running → StopPending (wait hint = the budget) → Stopped;
// Interrogate echoes the current status; PreShutdown is accepted.
func TestSvcHandler(t *testing.T) {
	for _, cmd := range []svc.Cmd{svc.Stop, svc.Shutdown, svc.PreShutdown} {
		var cancels atomic.Int32
		h := &tenantService{budget: 7 * time.Second, run: func(ctx context.Context) error {
			<-ctx.Done()
			cancels.Add(1)
			return nil
		}}
		r := make(chan svc.ChangeRequest)
		s := make(chan svc.Status, 16)
		done := make(chan struct{})
		go func() { h.Execute(nil, r, s); close(done) }()

		if st := <-s; st.State != svc.StartPending {
			t.Fatalf("%v: first status %v, want StartPending", cmd, st.State)
		}
		running := <-s
		if running.State != svc.Running || running.Accepts&svc.AcceptPreShutdown == 0 || running.Accepts&svc.AcceptStop == 0 || running.Accepts&svc.AcceptShutdown == 0 {
			t.Fatalf("%v: running status %+v", cmd, running)
		}
		r <- svc.ChangeRequest{Cmd: svc.Interrogate}
		if st := <-s; st != running {
			t.Errorf("%v: interrogate echoed %+v, want %+v", cmd, st, running)
		}
		r <- svc.ChangeRequest{Cmd: cmd}
		if st := <-s; st.State != svc.StopPending || st.WaitHint != 7000 {
			t.Errorf("%v: after the control %+v, want StopPending with a 7000 ms hint", cmd, st)
		}
		if st := <-s; st.State != svc.Stopped {
			t.Errorf("%v: final status %v, want Stopped", cmd, st.State)
		}
		<-done
		if n := cancels.Load(); n != 1 {
			t.Errorf("%v: serve's context cancelled %d times, want once", cmd, n)
		}
	}
}
