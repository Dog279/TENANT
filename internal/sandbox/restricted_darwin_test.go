//go:build darwin

package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/sandbox"
	"tenant/internal/sandbox/escapetest"
)

// TestEscape_HostDocumentsGap runs the containment suite against the unconfined
// host backend with expectContained=false: it must NOT contain the filesystem
// escapes, which documents exactly what the seam buys us once a restricted
// backend is in play.
func TestEscape_HostDocumentsGap(t *testing.T) {
	escapetest.Harness(t, sandbox.Host(), false)
}

// TestEscape_SeatbeltContains runs the same suite against the macOS Seatbelt
// restricted backend with expectContained=true: the filesystem escapes MUST be
// genuinely contained.
func TestEscape_SeatbeltContains(t *testing.T) {
	b, err := sandbox.Restricted()
	if err != nil {
		t.Fatalf("sandbox.Restricted() on darwin: %v", err)
	}
	if b.Isolation() != sandbox.IsolationProcess {
		t.Fatalf("restricted backend isolation = %v, want process", b.Isolation())
	}
	escapetest.Harness(t, b, true)
}

// TestSeatbelt_ReadWriteBoundary is a focused check that the boundary denies a
// read of a secret outside the roots, allows a write inside the work dir, and
// allows a legit read inside a ReadRoot.
func TestSeatbelt_ReadWriteBoundary(t *testing.T) {
	b, err := sandbox.Restricted()
	if err != nil {
		t.Fatalf("sandbox.Restricted(): %v", err)
	}

	base := t.TempDir()
	work := filepath.Join(base, "work")
	secret := filepath.Join(base, "secret")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	const sentinel = "SENTINEL-527ab1"
	secretFile := filepath.Join(secret, "secret.txt")
	if err := os.WriteFile(secretFile, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(work, "readable.txt")
	if err := os.WriteFile(readable, []byte("hello-inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := sandbox.Policy{
		WorkDir:    work,
		ReadRoots:  []string{work},
		WriteRoots: []string{work},
		DenyRoots:  []string{secret},
		Network:    sandbox.NetNone,
		Limits:     sandbox.Limits{Wall: 10 * time.Second, MaxOutputBytes: 8192},
		Enforce:    true,
	}
	ctx := context.Background()

	// 1. reading a secret OUTSIDE the roots is denied and does not leak.
	res, err := b.Run(ctx, sandbox.Command{Shell: "cat " + q(secretFile)}, pol)
	if err != nil {
		t.Fatalf("infra error: %v", err)
	}
	if !res.Contained() {
		t.Fatalf("reading the out-of-roots secret must be contained; exit=%d output=%q", res.ExitCode, res.Output)
	}
	if strings.Contains(res.Output, sentinel) {
		t.Fatalf("secret leaked despite containment: %q", res.Output)
	}

	// 2. writing INSIDE the work dir is allowed and the bytes land on disk.
	out := filepath.Join(work, "written.txt")
	res, err = b.Run(ctx, sandbox.Command{Shell: "echo boundary-ok > " + q(out)}, pol)
	if err != nil {
		t.Fatalf("infra error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("write inside the work dir must succeed; exit=%d output=%q", res.ExitCode, res.Output)
	}
	if got, err := os.ReadFile(out); err != nil || !strings.Contains(string(got), "boundary-ok") {
		t.Fatalf("expected written file with content, got err=%v content=%q", err, string(got))
	}

	// 3. a legit read INSIDE a ReadRoot works and returns the content.
	res, err = b.Run(ctx, sandbox.Command{Shell: "cat " + q(readable)}, pol)
	if err != nil {
		t.Fatalf("infra error: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "hello-inside") {
		t.Fatalf("legit read inside a ReadRoot must succeed with content; exit=%d output=%q", res.ExitCode, res.Output)
	}
}

// q single-quotes a path for POSIX sh.
func q(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
