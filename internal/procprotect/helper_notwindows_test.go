//go:build !windows

package procprotect

import "os"

// Only Windows needs another way out for the test process (refuse_windows_test.go).
func exit(code int) { os.Exit(code) }

// Only Windows has a console to keep open (console_windows.go).
func anchorIfAsked() {}

func victimBefore() {}

func squat() {}
