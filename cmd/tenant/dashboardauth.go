package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/secrets"
)

const dashboardAuthTokenFile = "dashboard-auth-token"

func dashboardAuthTokenPath(cfgDir string) string {
	return filepath.Join(cfgDir, dashboardAuthTokenFile)
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
