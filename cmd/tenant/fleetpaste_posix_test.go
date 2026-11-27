//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every line enroll prints for Linux and macOS, pasted into the real sh, puts
// the code where the installer or `tenant child install` reads it, unchanged:
// in TENANT_ENROLL_CODE through sudo, or on standard input. curl, sudo and
// tenant are stand-ins on the PATH that only say what they were given;
// nothing is fetched, installed or run as root.
func TestEnrollPasteInARealShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	t.Setenv("TENANT_UPDATE_REPO", "acme/tenant")
	bin := t.TempDir()
	stand := map[string]string{
		// The script curl "fetches" says what it was run with.
		"curl": `printf '%s\n' "$*" > "$BIN/curl.args"
printf '%s\n' 'printf "child=%s code=%s\n" "$TENANT_CHILD" "$TENANT_ENROLL_CODE"'`,
		"sudo": `printf '%s\n' "$*" > "$BIN/sudo.args"
while [ $# -gt 0 ]; do case $1 in -*) shift ;; *) break ;; esac; done
exec "$@"`,
		"tenant": `printf 'args=%s stdin=' "$*"; cat`,
	}
	for name, body := range stand {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range pasteCodes {
		for _, system := range []bool{true} { // a child is always the machine's own
			lines, _ := enrollPaste(code)
			for _, l := range lines {
				if !strings.HasPrefix(l.For, "Linux") {
					continue
				}
				_ = os.Remove(filepath.Join(bin, "sudo.args"))
				cmd := exec.Command(sh, "-s")
				cmd.Stdin = strings.NewReader(l.Line + "\n")
				cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + bin, "BIN=" + bin}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", l.Line[:40], err, out)
				}
				child, want := "user", ""
				if system {
					child = "system"
				}
				switch {
				case strings.Contains(l.Line, "install.sh"):
					want = "child=" + child + " code=" + code + "\n"
					args, _ := os.ReadFile(filepath.Join(bin, "curl.args"))
					if string(args) != "-fsSL https://github.com/acme/tenant/releases/latest/download/install.sh\n" {
						t.Errorf("curl was given %q", args)
					}
				case system:
					want = "args=child install --system stdin=" + code + "\n"
				default:
					want = "args=child install stdin=" + code + "\n"
				}
				if string(out) != want {
					t.Errorf("%q in sh gave %q, want %q", l.For, out, want)
				}
				args, err := os.ReadFile(filepath.Join(bin, "sudo.args"))
				if sudo := strings.Contains(l.Line, "sudo "); sudo != (err == nil) || sudo && string(args) != "--preserve-env=TENANT_CHILD,TENANT_ENROLL_CODE sh\n" {
					t.Errorf("%q: sudo was given %q (%v)", l.For, args, err)
				}
			}
		}
	}
}
