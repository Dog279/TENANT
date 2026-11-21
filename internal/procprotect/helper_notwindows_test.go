//go:build !windows

package procprotect

// Only Windows has a console to keep open (console_windows.go).
func anchorIfAsked() {}

func victimBefore() {}

func squat() {}
