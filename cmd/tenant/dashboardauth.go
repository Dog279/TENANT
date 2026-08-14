package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/dashboard"
)

const dashboardAuthTokenFile = "dashboard-auth-token"

func dashboardAuthTokenPath(cfgDir string) string {
	return filepath.Join(cfgDir, dashboardAuthTokenFile)
}

// resolveDashboardAuth returns an explicitly configured token when present.
// Otherwise it loads the generated default from the config directory, creating
// a cryptographically random owner-only token on first use.
func resolveDashboardAuth(cfgDir, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	path := dashboardAuthTokenPath(cfgDir)
	if b, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(b))
		if token == "" {
			return "", fmt.Errorf("dashboard auth token file %q is empty", path)
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read dashboard auth token: %w", err)
	}

	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return "", fmt.Errorf("create dashboard auth token directory: %w", err)
	}
	token := dashboard.GenerateToken()
	if err := atomicWrite(path, []byte(token+"\n")); err != nil {
		return "", fmt.Errorf("persist dashboard auth token: %w", err)
	}
	return token, nil
}
