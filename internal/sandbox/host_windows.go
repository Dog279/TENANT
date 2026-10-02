package sandbox

import "os/exec"

// detach is a no-op on Windows: there is no controlling terminal to escape.
func detach(*exec.Cmd) {}
