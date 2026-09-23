//go:build windows

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- pure unit tests (no WSL needed; run on any Windows) ---

func TestToLinuxPath(t *testing.T) {
	b := &wslBackend{info: wslInfo{mountPrefix: "/mnt"}}
	cases := []struct{ in, want string }{
		{`C:\a\b`, "/mnt/c/a/b"},
		{`D:\x`, "/mnt/d/x"},
		{`C:/forward/slash`, "/mnt/c/forward/slash"},
		{`E:\`, "/mnt/e"},
		{"/already/linux", "/already/linux"},
		{`relative\path`, `relative\path`}, // best-effort: left as-is
		{"", ""},
	}
	for _, c := range cases {
		if got := b.toLinuxPath(c.in); got != c.want {
			t.Errorf("toLinuxPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A custom automount root must be honoured.
	if got := (&wslBackend{info: wslInfo{mountPrefix: "/"}}).toLinuxPath(`C:\a`); got != "/c/a" {
		t.Errorf("custom mount prefix: got %q, want /c/a", got)
	}
}

func TestCommandLine(t *testing.T) {
	cases := []struct {
		cmd  Command
		want string
	}{
		{Command{Shell: "echo hi"}, "echo hi"},
		{Command{Args: []string{"cmd", "/C", "dir"}}, "dir"},
		{Command{Args: []string{"sh", "-c", "ls -la"}}, "ls -la"},
		{Command{Args: []string{"echo", "hi"}}, "echo hi"},
	}
	for _, c := range cases {
		if got := commandLine(c.cmd); got != c.want {
			t.Errorf("commandLine(%+v) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestDecodeWSLText(t *testing.T) {
	// UTF-16LE with BOM, as `wsl.exe -l -q` emits: "Ubuntu\r\nDebian\r\n".
	utf16 := []byte{0xFF, 0xFE}
	for _, r := range "Ubuntu\r\nDebian\r\n" {
		utf16 = append(utf16, byte(r), 0x00)
	}
	got := strings.ReplaceAll(string(decodeWSLText(utf16)), "\r", "")
	if got != "Ubuntu\nDebian\n" {
		t.Fatalf("decodeWSLText = %q, want Ubuntu/Debian lines", got)
	}
	// Plain UTF-8 passes through untouched.
	if s := string(decodeWSLText([]byte("Alpine\n"))); s != "Alpine\n" {
		t.Fatalf("utf-8 passthrough = %q", s)
	}
}

// --- live containment tests (skip when this host has no WSL sandbox) ---

func wslBackendOrSkip(t *testing.T) *wslBackend {
	t.Helper()
	b, err := Restricted()
	if err != nil {
		t.Skipf("no WSL restricted backend on this host: %v", err)
	}
	wb, ok := b.(*wslBackend)
	if !ok {
		t.Skipf("restricted backend is %T, not WSL", b)
	}
	return wb
}

func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestWSL_ProbeConfines(t *testing.T) {
	c := wslBackendOrSkip(t).Probe(context.Background())
	if c.Filesystem != EnforceFull || c.Network != EnforceFull {
		t.Errorf("WSL backend must enforce filesystem and network: %+v", c)
	}
	if c.Shell != "sh" {
		t.Errorf("shell = %q, want sh (commands run in WSL)", c.Shell)
	}
	if c.Isolation != IsolationProcess {
		t.Errorf("isolation = %v, want process", c.Isolation)
	}
}

// TestWSL_ContainsSecretAndAllowsWork: a canary secret outside the roots does not
// leak, while a file inside the work dir reads fine (the boundary contains
// without breaking legitimate work).
func TestWSL_ContainsSecretAndAllowsWork(t *testing.T) {
	wb := wslBackendOrSkip(t)
	work := t.TempDir()
	secret := t.TempDir() // a separate dir, outside the work/read/write roots
	const sentinel = "CANARY-9f3a1c7e-do-not-leak"
	if err := os.WriteFile(filepath.Join(work, "ok.txt"), []byte("inside-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "s.txt"), []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := Policy{
		WorkDir:    work,
		ReadRoots:  []string{work},
		WriteRoots: []string{work},
		DenyRoots:  []string{secret},
		Network:    NetNone,
		Limits:     Limits{Wall: 30 * time.Second, MaxOutputBytes: 64 << 10},
		Enforce:    true,
	}

	// Legitimate read inside the work dir must succeed.
	legit, err := wb.Run(context.Background(), Command{Shell: "cat " + sq(wb.toLinuxPath(filepath.Join(work, "ok.txt")))}, pol)
	if err != nil {
		t.Fatalf("legit read infra error: %v", err)
	}
	if legit.ExitCode != 0 || !strings.Contains(legit.Output, "inside-ok") {
		t.Fatalf("legit read of the work dir must succeed: exit=%d out=%q", legit.ExitCode, legit.Output)
	}

	// The out-of-roots secret must not come back.
	adv, err := wb.Run(context.Background(), Command{Shell: "cat " + sq(wb.toLinuxPath(filepath.Join(secret, "s.txt")))}, pol)
	if err != nil {
		t.Fatalf("secret read infra error: %v", err)
	}
	if strings.Contains(adv.Output, sentinel) {
		t.Fatalf("the sandbox leaked the out-of-roots secret: out=%q", adv.Output)
	}
	if adv.ExitCode == 0 {
		t.Fatalf("reading the denied secret should fail: exit=%d out=%q", adv.ExitCode, adv.Output)
	}
}

// TestWSL_ContainsWriteOutsideRoots: a write aimed outside the writable roots
// must not land on the host.
func TestWSL_ContainsWriteOutsideRoots(t *testing.T) {
	wb := wslBackendOrSkip(t)
	work := t.TempDir()
	outside := t.TempDir()
	pol := Policy{
		WorkDir:    work,
		ReadRoots:  []string{work},
		WriteRoots: []string{work},
		Network:    NetNone,
		Limits:     Limits{Wall: 30 * time.Second, MaxOutputBytes: 64 << 10},
		Enforce:    true,
	}
	target := filepath.Join(outside, "backdoor.txt")
	_, err := wb.Run(context.Background(), Command{Shell: "echo pwned > " + sq(wb.toLinuxPath(target))}, pol)
	if err != nil {
		t.Fatalf("infra error: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		_ = os.Remove(target)
		t.Fatalf("a write outside the writable roots landed on the host at %s (stat err=%v)", target, err)
	}
}

// TestWSL_ContainsNetwork: with NetNone, an outbound connection cannot be made.
func TestWSL_ContainsNetwork(t *testing.T) {
	wb := wslBackendOrSkip(t)
	if strings.TrimSpace(runWSL(wb.info.wslExe, wb.info.distro, "sh", "-c", "command -v bash")) == "" {
		t.Skip("bash not present in the distro; skipping the /dev/tcp network probe")
	}
	work := t.TempDir()
	pol := Policy{
		WorkDir: work, ReadRoots: []string{work}, WriteRoots: []string{work},
		Network: NetNone, Limits: Limits{Wall: 20 * time.Second, MaxOutputBytes: 4096}, Enforce: true,
	}
	res, err := wb.Run(context.Background(),
		Command{Shell: `bash -c 'exec 3<>/dev/tcp/1.1.1.1/53 && echo CONNECTED' 2>&1`}, pol)
	if err != nil {
		t.Fatalf("infra error: %v", err)
	}
	if strings.Contains(res.Output, "CONNECTED") {
		t.Fatalf("network must be cut under NetNone, but a connection succeeded: out=%q", res.Output)
	}
}
