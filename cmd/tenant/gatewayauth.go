package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"

	"tenant/internal/dashboard"
	"tenant/internal/secrets"
)

// The `mcp-memory --sse-addr` gateway's bearer token. The gateway serves
// memory, and with --tools the whole toolset with no approval prompt, to
// whoever can reach its port: on a loopback bind that is every program on the
// machine, so it asks for a token the way the dashboard does. The token is a
// file of its own, not the dashboard's: a client given the gateway must not
// thereby hold the dashboard.
const gatewayAuthTokenFile = "gateway-auth-token"

func gatewayAuthTokenPath(cfgDir string) string {
	return filepath.Join(cfgDir, gatewayAuthTokenFile)
}

// resolveGatewayAuth loads the gateway's token from the config dir, creating
// a random owner-only one on first use. Creation is exclusive, as for the
// dashboard's (resolveDashboardAuth): two gateways started at once agree on
// one token.
func resolveGatewayAuth(cfgDir string) (string, error) {
	path := gatewayAuthTokenPath(cfgDir)
	token, err := readGatewayAuthToken(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return token, err
	}
	token = dashboard.GenerateToken()
	created, err := secrets.CreateExclusive(path, []byte(token+"\n"))
	if err != nil {
		return "", fmt.Errorf("persist gateway auth token: %w", err)
	}
	if !created {
		return readGatewayAuthToken(path)
	}
	return token, nil
}

// readGatewayAuthToken reads the persisted token, waiting out a claimed but
// unwritten file (readClaimed). An empty file is an error, never a gateway
// that accepts an empty token.
func readGatewayAuthToken(path string) (string, error) {
	b, err := readClaimed(path, secrets.ReadFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("read gateway auth token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("gateway auth token file %q is empty: delete it and start the gateway again to generate one", path)
	}
	return token, nil
}

// gatewayTokenHint says where the gateway's token lives and how to show it,
// never the token itself: the line goes to a log.
func gatewayTokenHint(cfgDir string) string {
	path := gatewayAuthTokenPath(cfgDir)
	show := "cat"
	if runtime.GOOS == "windows" {
		show = "type"
	}
	return "clients send it as \"Authorization: Bearer <token>\"; the token is in " + path + " (show it: " + show + ` "` + path + `")`
}
