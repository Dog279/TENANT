package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TEN-401: the sweep removes a leftover Chrome profile folder, and keeps one a
// running Chrome holds, one that is brand new, and anything not chromedp's.
func TestSweepChromeProfiles(t *testing.T) {
	tmp := t.TempDir()
	old := time.Now().Add(-time.Hour)
	mk := func(name string, mtime time.Time) string {
		d := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Join(d, "Default"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(d, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return d
	}
	stale := mk("chromedp-runner111", old)
	held := mk("chromedp-runner222", old)
	fresh := mk("chromedp-runner333", time.Now())
	other := mk("someone-else", old)

	n := sweepChromeProfiles(tmp, func(d string) bool { return d == held })
	if n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the leftover profile should be gone")
	}
	for _, d := range []string{held, fresh, other} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s should be kept: %v", filepath.Base(d), err)
		}
	}
}
