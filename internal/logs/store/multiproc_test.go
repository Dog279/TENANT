package store

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

// TestMain lets the test binary stand in for another Tenant process writing
// the same log book: with TENANT_LOGBOOK_HELPER=writer it commits
// TENANT_LOGBOOK_N events as writer TENANT_LOGBOOK_W, then exits.
func TestMain(m *testing.M) {
	if os.Getenv("TENANT_LOGBOOK_HELPER") == "writer" {
		if err := helperWriter(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperWriter() error {
	w, _ := strconv.Atoi(os.Getenv("TENANT_LOGBOOK_W"))
	n, _ := strconv.Atoi(os.Getenv("TENANT_LOGBOOK_N"))
	s, err := Open(os.Getenv("TENANT_LOGBOOK_DB"), Options{})
	if err != nil {
		return err
	}
	defer s.Close()
	for i := 0; i < n; {
		size := min(1+(i*37+w*101)%512, n-i) // uneven batches, up to the pipeline's 512
		batch := make([]event.Event, size)
		for j := range batch {
			batch[j] = ev("multi", fmt.Sprintf("writer %d event %d", w, i+j), "w", int64(w), "i", int64(i+j))
		}
		// A commit is all or nothing, so a batch that failed (a writer
		// starved past busy_timeout, as under -race) is simply retried, as the
		// pipeline's writer will (TEN-373).
		for tries := 1; ; tries++ {
			err := s.Commit(ctx, batch)
			if err == nil {
				break
			}
			if tries == 5 {
				return err
			}
		}
		i += size
	}
	return nil
}

// No missed rows: three writer processes commit at once while a reader tails
// from a cursor, woken by its subscription; it sees every row exactly once,
// in seq order. TENANT_LOGBOOK_SOAK=1 runs the plan's 1M events.
func TestNoMissedRowsAcrossProcesses(t *testing.T) {
	const writers = 3
	perWriter, timeout := 20_000, 2*time.Minute
	if os.Getenv("TENANT_LOGBOOK_SOAK") == "1" {
		perWriter, timeout = 1_000_000/writers+1, 30*time.Minute
	} else if testing.Short() {
		perWriter = 2_000
	}
	path := tempDB(t)
	s := open(t, path, "")
	wake, cancel := s.Subscribe()
	defer cancel()

	done := make(chan error, writers)
	for w := range writers {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), "TENANT_LOGBOOK_HELPER=writer", "TENANT_LOGBOOK_DB="+path,
			"TENANT_LOGBOOK_W="+strconv.Itoa(w), "TENANT_LOGBOOK_N="+strconv.Itoa(perWriter))
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- cmd.Wait() }()
	}

	seen := make([][]bool, writers)
	for w := range seen {
		seen[w] = make([]bool, perWriter)
	}
	var cur Cursor
	var last uint64
	total, exited := 0, 0
	deadline := time.After(timeout)
	for total < writers*perWriter {
		evs, next, restarted, err := s.Since(ctx, cur, 5000)
		if err != nil || restarted {
			t.Fatalf("Since(%v) = restarted %v, %v", cur, restarted, err)
		}
		cur = next
		for _, e := range evs {
			if e.Seq <= last {
				t.Fatalf("seq %d after %d", e.Seq, last)
			}
			last = e.Seq
			w, i := e.Data[0].Value.(int64), e.Data[1].Value.(int64)
			if seen[w][i] {
				t.Fatalf("writer %d event %d seen twice", w, i)
			}
			seen[w][i] = true
			total++
		}
		if len(evs) > 0 {
			continue
		}
		select {
		case <-wake:
		case err := <-done:
			if err != nil {
				t.Fatalf("writer process: %v", err)
			}
			exited++
		case <-deadline:
			t.Fatalf("saw %d of %d events before the deadline", total, writers*perWriter)
		}
	}
	for ; exited < writers; exited++ {
		if err := <-done; err != nil {
			t.Fatalf("writer process: %v", err)
		}
	}
	if evs, _, _, _ := s.Since(ctx, cur, 0); len(evs) != 0 {
		t.Errorf("%d rows after every writer's last one", len(evs))
	}
}
