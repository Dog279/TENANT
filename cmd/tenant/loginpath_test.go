package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// Under launchd's bare PATH, Tenant still finds what a login shell would.
func TestFillLoginPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	fillLoginPath()
	p := os.Getenv("PATH")
	if !strings.HasPrefix(p, "/usr/bin:/bin") || !strings.Contains(p, "/usr/sbin") {
		t.Fatalf("PATH = %q: want the inherited entries first, then path_helper's", p)
	}
	if _, err := os.Stat("/opt/homebrew/bin"); err == nil && !strings.Contains(p, "/opt/homebrew/bin") {
		t.Errorf("PATH = %q: want Homebrew's bin", p)
	}
}
