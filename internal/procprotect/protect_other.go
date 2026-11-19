//go:build !linux

package procprotect

// No controls on this system.
func protect() []Control { return nil }

func system() []string { return nil }
