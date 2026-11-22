//go:build !windows

package onepassword

import "os/exec"

// noWindow is a no-op off Windows, where a child has no console window.
func noWindow(*exec.Cmd) {}
