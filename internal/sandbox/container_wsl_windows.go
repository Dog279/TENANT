//go:build windows

package sandbox

import (
	"context"
	"os/exec"
	"time"
)

// probeWSL reads the WSL release and kernel from `wsl --version`, whose output
// is UTF-16 (W1-1 / TEN-455). Both are "" when WSL is not installed or the
// command fails: a headless engine over ssh needs no WSL, and the engine floors
// still apply. The caller gates on what came back (checkWSLFloors).
func probeWSLOS(ctx context.Context) (wsl, kernel string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wsl", "--version").Output()
	if err != nil {
		return "", ""
	}
	return parseWSLVersion(decodeConsoleOutput(out))
}
