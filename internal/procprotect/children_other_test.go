//go:build !windows

package procprotect_test

import "testing"

// Only Windows has the job. On Linux chromedp has the kernel kill the browser
// when the process that started it dies; on macOS the test's Cleanup is what
// closes it.
func endTreeWithThisProcess(*testing.T) {}
