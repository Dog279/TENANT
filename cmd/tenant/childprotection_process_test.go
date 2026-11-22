package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet"
)

// A fleet child has no log book: `tenant child` resolves its own directory
// and never passes commonFlags.resolveDirs, where every other command files a
// process protection that is not in force (logUnprotected). `child run` says
// it where a child keeps its record: a warning per control in its own log
// (standard error) and a record per control in its audit log, whose line goes
// to the system's log too. The operator's short commands say it on stderr at
// a terminal. With every control in force none of them says anything.
func TestAChildWithAMissingControlSaysSo(t *testing.T) {
	first, second := forcedControls(t) // skips where there are no controls
	bin := protectTestBinary(t)
	removeTestExclusion(t, bin) // the start with every control in force writes it
	u := newTenantUnderTest(t, bin)
	dir := filepath.Join(u.home, "child")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A child.json that does not load: the child opens its audit log, holds
	// no link and says so, which is the sign that its start is behind it.
	if err := os.WriteFile(fleet.ChildPath(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	forced := "TENANT_TEST_FORCE_FAIL=" + first + "=forced by the test;" + second + "=forced by the test"

	run := func(env ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := u.command(ctx, env, "child", "run", "--dir", dir)
		var said syncBuffer
		cmd.Stdout, cmd.Stderr = &said, &said
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ended := make(chan struct{})
		go func() { _ = cmd.Wait(); close(ended) }()
		stop := func() { _ = cmd.Process.Kill(); <-ended }
		defer stop() // also when the test fails
		for deadline := time.Now().Add(60 * time.Second); !strings.Contains(said.String(), "no link can be held"); time.Sleep(50 * time.Millisecond) {
			select {
			case <-ended:
				t.Fatalf("the child ended at its start:\n%s", said.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("the child did not get past its start:\n%s", said.String())
			}
		}
		return said.String()
	}
	unprotected := func() []fleet.AuditRecord {
		t.Helper()
		if _, err := fleet.VerifyAudit(dir); err != nil {
			t.Fatal(err)
		}
		var out []fleet.AuditRecord
		for _, l := range bytes.Split(bytes.TrimSpace(mustRead(t, filepath.Join(fleet.AuditDir(dir), "audit.jsonl"))), []byte("\n")) {
			var r fleet.AuditRecord
			if json.Unmarshal(l, &r) == nil && r.Event == fleet.AuditUnprotected {
				out = append(out, r)
			}
		}
		return out
	}
	warned := func(said, id string) int {
		n := 0
		for _, l := range strings.Split(said, "\n") {
			if strings.Contains(l, "level=WARN") && strings.Contains(l, "process protection: running without") &&
				strings.Contains(l, "control="+id) && strings.Contains(l, `reason="forced by the test"`) {
				n++
			}
		}
		return n
	}

	said := run(forced)
	recs := unprotected()
	for _, id := range []string{first, second} {
		if n := warned(said, id); n != 1 {
			t.Errorf("child run with %s forced off: the warning is in its log %d times, want once:\n%s", id, n, said)
		}
		n := 0
		for _, r := range recs {
			if r.Control == id && r.Note == "forced by the test" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("child run with %s forced off: %d audit records name it, want 1: %+v", id, n, recs)
		}
		if n := strings.Count(strings.Join(u.systemLogLines(t), "\n"), "event=unprotected control="+id+" "); n != 1 {
			t.Errorf("the system log has %d lines for %s, want 1: %q", n, id, u.systemLogLines(t))
		}
	}
	// A control forced off can take another with it (Windows: the console
	// host's, without the process's access list): each one once, both places.
	seen := map[string]bool{}
	for _, r := range recs {
		if seen[r.Control] || strings.Count(said, "control="+r.Control+" ") != 1 {
			t.Errorf("control %s: in the audit log twice, or not once in the child's log:\n%s", r.Control, said)
		}
		seen[r.Control] = true
	}
	if n := strings.Count(said, "running without"); n != len(recs) {
		t.Errorf("%d warnings and %d audit records", n, len(recs))
	}

	// Every control in force: nothing in the log, no record.
	if said := run(); strings.Contains(said, "running without") {
		t.Errorf("child run with every control in force says:\n%s", said)
	}
	if n := len(unprotected()); n != len(recs) {
		t.Errorf("after a start with every control in force there are %d records of a missing control, want the %d from before", n, len(recs))
	}

	// The operator's short commands: the line on stderr at a terminal, once
	// per control; nothing with every control in force, or with no terminal.
	line := "process protection: running without " + first + ": forced by the test. `tenant doctor` says what that leaves open"
	for _, tc := range []struct {
		env  []string
		want int
	}{
		{[]string{forced, "TENANT_TEST_TERMINAL=1"}, 1},
		{[]string{forced}, 0},
		{[]string{"TENANT_TEST_TERMINAL=1"}, 0},
	} {
		_, _, stderr := u.exits(t, tc.env, "", "child", "status", "--dir", dir)
		if n := strings.Count(stderr, line); n != tc.want {
			t.Errorf("child status with %v: the line is on stderr %d times, want %d:\n%s", tc.env, n, tc.want, stderr)
		}
	}
}
