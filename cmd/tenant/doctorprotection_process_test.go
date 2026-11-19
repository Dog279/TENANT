package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// Doctor's "process protection" is about the Tenant that is running (TEN-535
// hardening). The tester's finding: with a serve up, doctor described its own
// short-lived process, and went on calling a control in force after another
// program of the user had undone it under the serve.
//
// This runs the real binary as serve, with its dashboard on a port of its
// own, and asks with doctor's check from this process, which is not
// protected at all: whatever the answer says of a control is the serve's.
// Then a second program (this test) undoes one control of the serve from
// outside, the way undoFromOutside says for this system, and asks again.
func TestDoctorDescribesTheRunningServe(t *testing.T) {
	forcedControls(t) // skips where there are no controls
	bin := protectTestBinary(t)
	u := newTenantUnderTest(t, bin)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	e := &doctorEnv{c: &commonFlags{cfgDir: u.cfg, dataDir: u.data, agent: "main", auditOnly: true},
		lc: &launchConfig{Dashboard: dashboardConfig{Addr: addr}}}
	ask := func() checkResult { return checkProcessProtection(context.Background(), e) }
	const aStart = "This is what a start would get"

	if r := ask(); !strings.Contains(r.Detail, "No running Tenant answered") || !strings.Contains(r.Detail, aStart) {
		t.Errorf("with no serve running, doctor does not say whose controls these are:\n%s", r.Detail)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prepare, undo, found := undoFromOutside(t, bin)
	cmd := u.command(ctx, prepare, "serve", "--backend", "echo", "--data", u.data, "--config", u.cfg, "--self-improve=false", "--dashboard-addr", addr)
	var errb syncBuffer
	cmd.Stderr = &errb
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	t.Cleanup(func() { // also when the test fails
		_ = cmd.Process.Kill()
		<-ended
	})
	up := make(chan bool, 1)
	go func() {
		defer close(ended)
		told := false
		for sc := bufio.NewScanner(pipe); sc.Scan(); {
			if !told && strings.Contains(sc.Text(), "running 24/7") {
				told = true
				up <- true
			}
		}
		if !told {
			up <- false
		}
		_ = cmd.Wait()
	}()
	if !<-up {
		t.Fatalf("serve did not come up:\n%s", errb.String())
	}
	whose := fmt.Sprintf("The Tenant running at %s (process %d,", addr, cmd.Process.Pid)

	var r checkResult
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) { // the dashboard listens a moment after the line
		if r = ask(); strings.Contains(r.Detail, whose) || time.Now().After(deadline) {
			break
		}
	}
	if r.Status != statusOK || !strings.Contains(r.Detail, whose) || strings.Contains(r.Detail, aStart) || strings.Contains(r.Detail, "OFF:") || !strings.Contains(r.Detail, "on:  ") {
		t.Fatalf("with a serve running (process %d), doctor says (%v):\n%s", cmd.Process.Pid, r.Status, r.Detail)
	}
	t.Logf("doctor, of the running serve:\n%s", r.Detail)

	if undo == nil {
		t.Log("NOT SHOWN HERE: no control of a running process can be undone from outside by this test on this system")
		return
	}
	undo(t, cmd.Process.Pid)
	r = ask()
	want := "it was in force when Tenant started and is not any more: " + found
	if r.Status != statusWarn || !strings.Contains(r.Detail, whose) || !strings.Contains(r.Detail, "OFF: ") || !strings.Contains(r.Detail, want) ||
		!strings.Contains(r.Detail, "A restart of Tenant applies it again") || !strings.Contains(r.Fix, "restart") {
		t.Errorf("with a control of the serve undone from outside, doctor says (%v):\n%s\nfix: %s", r.Status, r.Detail, r.Fix)
	}
	t.Logf("doctor, after the control was undone from outside:\n%s", r.Detail)
}
