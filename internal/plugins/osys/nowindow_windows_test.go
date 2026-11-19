package osys

import (
	"os/exec"
	"testing"
)

// checkNoWindow asserts c is created with a console that has no window, and
// neither with a new console nor detached from any, which is what makes
// windows open further down.
func checkNoWindow(t *testing.T, c *exec.Cmd) {
	t.Helper()
	const createNewConsole, detachedProcess = 0x00000010, 0x00000008
	a := c.SysProcAttr
	if a == nil || a.CreationFlags&createNoWindow == 0 || !a.HideWindow || a.CreationFlags&(createNewConsole|detachedProcess) != 0 {
		t.Fatalf("created with %+v", a)
	}
}
