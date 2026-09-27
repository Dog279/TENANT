package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"tenant/internal/dashboard"
	"tenant/internal/secrets"
)

const dashboardAuthTokenFile = "dashboard-auth-token"

func dashboardAuthTokenPath(cfgDir string) string {
	return filepath.Join(cfgDir, dashboardAuthTokenFile)
}

// dashboardServingLine is the status line for a started dashboard. It goes to
// the log book, so it says where the auth token lives and how to show it,
// never the token itself (TEN-360).
func dashboardServingLine(addr, cfgDir string, lc *launchConfig) string {
	return "dashboard: serving on http://" + addr + "; " + dashboardTokenHint(cfgDir, lc)
}

// dashboardTokenBanner is serve's start-up line about the token. It prints the
// token only to a terminal: launchd and systemd capture a daemon's stdout to
// files nobody rotates.
func dashboardTokenBanner(token, cfgDir string, lc *launchConfig, tty bool) string {
	if tty {
		return "dashboard auth token: " + token
	}
	return "dashboard: " + dashboardTokenHint(cfgDir, lc)
}

// dashboardTokenHint says where the dashboard's auth token lives and how to
// show it.
func dashboardTokenHint(cfgDir string, lc *launchConfig) string {
	if lc != nil && lc.Dashboard.Auth != "" {
		return "the auth token is dashboard.auth in " + launchConfigPath(cfgDir)
	}
	path := dashboardAuthTokenPath(cfgDir)
	show := "cat"
	if runtime.GOOS == "windows" {
		show = "type"
	}
	return "the auth token is in " + path + " (show it: " + show + ` "` + path + `")`
}

func stdoutIsTerminal() bool {
	fd := os.Stdout.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// resolveDashboardAuth returns an explicitly configured token when present.
// Otherwise it loads the generated default from the config directory, creating
// a cryptographically random owner-only token on first use. Creation is
// exclusive: when `tenant serve` and `tenant attach` race on a fresh install,
// the loser reads the winner's token instead of replacing it, so the running
// server and later clients agree (TEN-332).
func resolveDashboardAuth(cfgDir, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	path := dashboardAuthTokenPath(cfgDir)
	token, err := readDashboardAuthToken(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return token, err
	}
	token = dashboard.GenerateToken()
	created, err := secrets.CreateExclusive(path, []byte(token+"\n"))
	if err != nil {
		return "", fmt.Errorf("persist dashboard auth token: %w", err)
	}
	if !created {
		return readDashboardAuthToken(path)
	}
	return token, nil
}

// dashboardClientToken is the token a local client (`tenant log`, doctor)
// presents to the hub: config dashboard.auth when set, else the generated
// token file a default install serves with; "" when neither exists yet. A
// read-only audit (plain doctor) peeks at the file rather than hardening it.
func dashboardClientToken(cfgDir string, lc *launchConfig, audit bool) (string, error) {
	if lc != nil && lc.Dashboard.Auth != "" {
		return lc.Dashboard.Auth, nil
	}
	read := secrets.ReadFile
	if audit {
		read = secrets.Peek
	}
	b, err := read(dashboardAuthTokenPath(cfgDir))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read dashboard auth token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// readDashboardAuthToken reads a persisted token. A present but empty file is
// re-read for up to a second: without hard links, secrets.CreateExclusive
// claims the name before it writes the token.
func readDashboardAuthToken(path string) (string, error) {
	for attempt := 1; ; attempt++ {
		b, err := secrets.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err != nil {
			return "", fmt.Errorf("read dashboard auth token: %w", err)
		}
		if token := strings.TrimSpace(string(b)); token != "" {
			return token, nil
		}
		if attempt == 20 {
			return "", fmt.Errorf("dashboard auth token file %q is empty", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
