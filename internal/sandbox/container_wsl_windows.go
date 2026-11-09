//go:build windows

package sandbox

import (
	"context"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// probeWSL reads the WSL release and kernel from `wsl --version`, whose output
// is UTF-16 (W1-1 / TEN-455). Both are "" when WSL is not installed or the
// command fails: a headless engine over ssh needs no WSL, and the engine floors
// still apply. The caller gates on what came back (checkWSLFloors).
func probeWSLOS(ctx context.Context) (wsl, kernel string) {
	exe, err := wslExePath()
	if err != nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "--version").Output()
	if err != nil {
		return "", ""
	}
	return parseWSLVersion(decodeConsoleOutput(out))
}

// wslExePath is wsl.exe in the Windows system directory, as an absolute path.
// The directory comes from the OS itself, not from PATH or %SystemRoot%, so a
// wsl.exe planted earlier on PATH (or a rewritten environment) can neither run
// in Tenant's place nor answer the version floors with numbers of its choosing.
func wslExePath() (string, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wsl.exe"), nil
}
