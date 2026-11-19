package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"tenant/internal/datalock"
)

// The environment that turns the test binary into a second Tenant process
// (see TestMain).
const (
	helperEnv   = "TENANT_FLEET_HELPER"
	helperDir   = "TENANT_FLEET_DIR"
	helperIndex = "TENANT_FLEET_W"
	helperCount = "TENANT_FLEET_N"
	helperOwns  = "TENANT_FLEET_ID"
	helperPin   = "TENANT_FLEET_FP"
)

func helperVersion(w, i int) string { return fmt.Sprintf("w%d.%d", w, i) }
func helperName(w, i int) string    { return fmt.Sprintf("w%d-n%d", w, i) }

// unlessBusy retries a change while the file's lock is held by the other
// processes for longer than one wait for it lasts. Giving up on a busy lock
// is an error the caller is told about, not a lost change, so trying again
// is what a caller does.
func unlessBusy(change func() error) error {
	for start := time.Now(); ; {
		err := change()
		if !errors.Is(err, datalock.ErrBusy) || time.Since(start) > time.Minute {
			return err
		}
	}
}

// helperProcess is a Tenant process, like the command line beside a running
// master, that changes fleet.json as fast as it can: N times it records a
// new version for the one machine it owns and adds one enrollment code to
// the list every process shares.
func helperProcess() error {
	// The test holds the other end of stdin. When it goes away, however it
	// goes, so does this process; and it never outlives two minutes.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(3)
	}()
	time.AfterFunc(2*time.Minute, func() { os.Exit(4) })

	w, _ := strconv.Atoi(os.Getenv(helperIndex))
	n, _ := strconv.Atoi(os.Getenv(helperCount))
	id, pin := os.Getenv(helperOwns), os.Getenv(helperPin)
	s := Open(os.Getenv(helperDir))
	for i := range n {
		version := helperVersion(w, i)
		if err := unlessBusy(func() error {
			return s.SetReported(id, Reported{OS: "linux", Ceiling: "full", Version: version})
		}); err != nil {
			return fmt.Errorf("writer %d, change %d: %w", w, i, err)
		}
		// Nobody else writes this machine's version. If it is not what was
		// just saved, another process saved over it from an older copy.
		got, err := s.Machine(id)
		if err != nil {
			return fmt.Errorf("writer %d, change %d: read back: %w", w, i, err)
		}
		if got.Reported.Version != version {
			return fmt.Errorf("writer %d: lost update: saved version %q, the file says %q", w, version, got.Reported.Version)
		}
		if err := unlessBusy(func() error {
			_, err := s.CreateEnrollment(pin, helperName(w, i), time.Hour, "")
			return err
		}); err != nil {
			return fmt.Errorf("writer %d, code %d: %w", w, i, err)
		}
	}
	return nil
}

// Separate processes change fleet.json at once, the way `tenant fleet` and a
// running master do: three write as fast as they can while this one revokes
// machines. Every change of every process is in the file at the end, no
// revoke is ever undone, and no reader ever sees a file it cannot parse.
func TestProcessesNeverLoseEachOthersChanges(t *testing.T) {
	const writers = 3
	n, revokes := 30, 10
	if testing.Short() {
		n, revokes = 8, 4
	}
	m := newMaster(t)
	owned := make([]Machine, writers)
	for w := range owned {
		owned[w], _ = m.add(t, fmt.Sprint("own-", w), StateConfirmed)
	}
	doomed := make([]Machine, revokes)
	for i := range doomed {
		doomed[i], _ = m.add(t, fmt.Sprint("doomed-", i), StateConfirmed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The writer processes. Each is stopped by this test whatever happens:
	// by its stdin closing, by the kill in Cleanup, and by ctx.
	type helper struct {
		stderr bytes.Buffer
		exited chan struct{}
		err    error
	}
	helpers := make([]*helper, writers)
	allExited := make(chan struct{})
	var running sync.WaitGroup
	for w := range helpers {
		h := &helper{exited: make(chan struct{})}
		helpers[w] = h
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), helperEnv+"=writer", helperDir+"="+m.dir, helperIndex+"="+strconv.Itoa(w),
			helperCount+"="+strconv.Itoa(n), helperOwns+"="+owned[w].ID, helperPin+"="+m.id.Fingerprint)
		cmd.Stderr = &h.stderr
		cmd.WaitDelay = 5 * time.Second
		noWindow(cmd)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		running.Add(1)
		go func() {
			defer running.Done()
			h.err = cmd.Wait()
			close(h.exited)
		}()
		t.Cleanup(func() {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			<-h.exited
		})
	}
	go func() { running.Wait(); close(allExited) }()

	// A reader that looks all the time, as the running master does. On
	// POSIX it also reads the file with no lock at all: a save replaces the
	// file in one step, so even that never sees half of one.
	path := StorePath(m.dir)
	stop := make(chan struct{})
	var (
		watcher  sync.WaitGroup
		looks    int
		firstBad error
	)
	stopWatcher := sync.OnceFunc(func() { close(stop); watcher.Wait() })
	t.Cleanup(stopWatcher)
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		reader := Open(m.dir)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			looks++
			if err := reader.Err(); err != nil && firstBad == nil {
				firstBad = fmt.Errorf("look %d: %w", looks, err)
			}
			if runtime.GOOS != "windows" {
				if b, err := os.ReadFile(path); err == nil {
					if _, err := parseStore(b); err != nil && firstBad == nil {
						firstBad = fmt.Errorf("look %d, read without the lock: %w", looks, err)
					}
				}
			}
		}
	}()

	// Revoke, spread over the writers' run by how far they have got.
	progress := Open(m.dir)
	written := func() int {
		d, _, err := progress.view()
		if err != nil {
			return 0
		}
		count := 0
		for _, e := range d.Enrollments {
			if e.UsedBy == "" {
				count++
			}
		}
		return count
	}
	total := writers * n
	for k, victim := range doomed {
		for waiting := true; waiting && written() < k*total/revokes; {
			select {
			case <-allExited:
				waiting = false
			case <-ctx.Done():
				t.Fatal("the writers did not finish in time")
			case <-time.After(2 * time.Millisecond):
			}
		}
		if err := unlessBusy(func() error { return m.store.Revoke(victim.ID) }); err != nil {
			t.Fatalf("revoke %d: %v", k, err)
		}
		// Seen from a process that starts now, every revoke so far stands.
		fresh := Open(m.dir)
		for _, earlier := range doomed[:k+1] {
			if err := fresh.Check(earlier.ID); !errors.Is(err, ErrRevoked) {
				t.Fatalf("after revoke %d: machine %s: Check = %v, want it revoked", k, earlier.Name, err)
			}
		}
	}

	select {
	case <-allExited:
	case <-ctx.Done():
		t.Fatal("the writers did not finish in time")
	}
	stopWatcher()
	for w, h := range helpers {
		if h.err != nil {
			t.Fatalf("writer %d: %v\n%s", w, h.err, h.stderr.String())
		}
	}
	if firstBad != nil {
		t.Errorf("a reader saw a fleet.json it could not use: %v", firstBad)
	}
	if looks == 0 {
		t.Error("the reader never looked")
	}

	// Everything every process did is there.
	final := Open(m.dir)
	d, _, err := final.view()
	if err != nil {
		t.Fatal(err)
	}
	for _, victim := range doomed {
		if got := d.Machines[victim.ID]; got == nil || got.State != StateRevoked {
			t.Errorf("machine %s is not revoked at the end: a revoke was lost", victim.Name)
		}
	}
	for w, mach := range owned {
		got := d.Machines[mach.ID]
		if got == nil || got.State != StateConfirmed || got.Reported.Version != helperVersion(w, n-1) {
			t.Errorf("writer %d's machine at the end: %+v, want version %s", w, got, helperVersion(w, n-1))
		}
	}
	codes := map[string]int{}
	for _, e := range d.Enrollments {
		codes[e.Name]++
	}
	for w := range writers {
		for i := range n {
			if codes[helperName(w, i)] != 1 {
				t.Errorf("code %s is in the file %d times, want once: a change was lost", helperName(w, i), codes[helperName(w, i)])
			}
		}
	}
	t.Logf("%d changes from %d processes and %d revokes; the reader looked %d times", 2*total, writers, revokes, looks)
}
