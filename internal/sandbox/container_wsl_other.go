//go:build !windows

package sandbox

import "context"

// probeWSL has nothing to read off Windows: the engine runs on the host kernel
// (Linux) or in Docker Desktop's own VM (macOS, W3-3).
func probeWSLOS(context.Context) (wsl, kernel string) { return "", "" }
