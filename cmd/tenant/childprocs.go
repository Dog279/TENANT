package main

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// sweepChromeProfiles removes the web bridge's leftover Chrome profile
// folders (chromedp-runner*) under tmp that no running Chrome holds (TEN-401).
// A hard-killed Tenant never ran the session's cleanup, so each kill left one
// behind. inUse reports whether a running Chrome still holds dir. A folder
// touched in the last two minutes is skipped: another Tenant's Chrome may be
// starting in it and not hold its lock yet.
func sweepChromeProfiles(tmp string, inUse func(dir string) bool) int {
	dirs, _ := filepath.Glob(filepath.Join(tmp, "chromedp-runner*"))
	n := 0
	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() || time.Since(fi.ModTime()) < 2*time.Minute || inUse(d) {
			continue
		}
		if os.RemoveAll(d) == nil {
			n++
		}
	}
	if n > 0 {
		sysChromeProfilesSwept.Log(context.Background(), "n", n, "dir", tmp)
	}
	return n
}
