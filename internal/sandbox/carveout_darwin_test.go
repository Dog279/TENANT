//go:build darwin

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func seatbeltOrSkip(t *testing.T) Backend {
	t.Helper()
	b, err := Restricted()
	if err != nil {
		t.Skipf("no restricted backend: %v", err)
	}
	return b
}

// Cron's work dir lives INSIDE the data dir, which is a deny root. The carve-out
// must keep the work dir usable while its denied siblings stay denied.
func TestSeatbelt_WorkDirInsideDenyRootIsCarvedOut(t *testing.T) {
	b := seatbeltOrSkip(t)
	data := t.TempDir()
	work := filepath.Join(data, "cron-work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "secret.txt"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := Policy{WorkDir: work, DenyRoots: []string{data}, Network: NetNone, Limits: Limits{Wall: 10 * time.Second}}
	ctx := context.Background()

	res, err := b.Run(ctx, Command{Shell: "echo hi > out.txt && cat out.txt"}, pol)
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Output) != "hi" {
		t.Fatalf("work dir under a deny root must stay usable: %+v err=%v", res, err)
	}
	res, err = b.Run(ctx, Command{Shell: "cat ../secret.txt"}, pol)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Contained() || strings.Contains(res.Output, "TOPSECRET") {
		t.Fatalf("a sibling inside the deny root must stay denied: %+v", res)
	}
}

// The carve-out must never reopen a deny root that sits inside the work dir, and
// a work dir that IS a deny root stays denied (fail closed).
func TestSeatbelt_CarveOutNeverReopensDenyRoot(t *testing.T) {
	b := seatbeltOrSkip(t)
	ctx := context.Background()

	work := t.TempDir()
	inner := filepath.Join(work, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "secret.txt"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := Policy{WorkDir: work, DenyRoots: []string{inner}, Network: NetNone, Limits: Limits{Wall: 10 * time.Second}}
	if res, _ := b.Run(ctx, Command{Shell: "cat inner/secret.txt"}, pol); !res.Contained() || strings.Contains(res.Output, "TOPSECRET") {
		t.Fatalf("a deny root inside the work dir must stay denied: %+v", res)
	}
	if res, _ := b.Run(ctx, Command{Shell: "echo ok > a.txt"}, pol); res.ExitCode != 0 {
		t.Fatalf("the rest of the work dir must stay writable: %+v", res)
	}

	same := t.TempDir()
	pol = Policy{WorkDir: same, DenyRoots: []string{same}, Network: NetNone, Limits: Limits{Wall: 10 * time.Second}}
	if res, _ := b.Run(ctx, Command{Shell: "echo x > f.txt"}, pol); res.ExitCode == 0 {
		t.Fatalf("a work dir equal to a deny root must fail closed: %+v", res)
	}
}

// A backgrounded child must not outlive the run, and must not hold the output
// pipe (and so the whole call) open until the wall deadline.
func TestSeatbelt_ReapsBackgroundProcesses(t *testing.T) {
	b := seatbeltOrSkip(t)
	pol := Policy{WorkDir: t.TempDir(), Network: NetNone, Limits: Limits{Wall: 20 * time.Second}}

	start := time.Now()
	res, err := b.Run(context.Background(), Command{Shell: "sleep 30 >/dev/null 2>&1 & echo $!"}, pol)
	if err != nil {
		t.Fatal(err)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(res.Output))
	if perr != nil {
		t.Fatalf("could not read background pid from %q", res.Output)
	}
	if !hasLimitViolation(res, "background") {
		t.Fatalf("reaping a straggler must be reported: %+v", res)
	}
	if !processGone(pid, 3*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("background pid %d outlived the run", pid)
	}

	res, err = b.Run(context.Background(), Command{Shell: "sleep 30 & echo started"}, pol)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "started") {
		t.Fatalf("a straggler holding the pipe must not turn success into failure: %+v err=%v", res, err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("runs took %s: a straggler held the output pipe open", d)
	}
}

func TestSeatbelt_ExitStatusMatchesExec(t *testing.T) {
	b := seatbeltOrSkip(t)
	res, err := b.Run(context.Background(), Command{Shell: "exit 3"}, Policy{WorkDir: t.TempDir(), Limits: Limits{Wall: 10 * time.Second}})
	if err != nil || res.ExitCode != 3 || res.ExitStatus != "exit status 3" {
		t.Fatalf("want exit 3 / \"exit status 3\", got %+v err=%v", res, err)
	}
}

func hasLimitViolation(r Result, substr string) bool {
	for _, v := range r.Violations {
		if v.Kind == "limit" && strings.Contains(v.Detail, substr) {
			return true
		}
	}
	return false
}

// processGone polls until pid no longer exists (an orphaned zombie is reaped by
// launchd shortly after it is killed).
func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
