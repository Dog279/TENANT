package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/peering"
)

// managedSecret is one at-rest secret file Tenant writes through the secrets
// package.
type managedSecret struct {
	label string // how doctor names it: the path under its config/data dir
	path  string
}

// managedSecrets is the authoritative list of Tenant's at-rest secret files
// under the config and data dirs, present or not (TEN-337). doctor audits the
// protection of each one and the file tools refuse to read any of them, so a
// new secret file belongs here (a plugin's, in its descriptor) rather than in
// either consumer; the X token cache was missing from both. Remote-MCP token caches are listed as found on
// disk; fileToolGuards also guards their whole directory. The list is
// complete apart from those caches even when it also returns an error.
func managedSecrets(cfgDir, dataDir string) ([]managedSecret, error) {
	var out []managedSecret
	var listErr error
	add := func(root, path string) {
		label, err := filepath.Rel(root, path)
		if err != nil {
			label = filepath.Base(path)
		}
		out = append(out, managedSecret{label: filepath.ToSlash(label), path: path})
	}
	if cfgDir != "" {
		for _, path := range []string{
			credentialsPath(cfgDir),
			dashboardAuthTokenPath(cfgDir),
			chainKeyPath(cfgDir),
		} {
			add(cfgDir, path)
		}
		for _, path := range pluginConfigSecrets(cfgDir) {
			add(cfgDir, path)
		}
		for _, path := range peering.AtRestPaths(cfgDir) {
			add(cfgDir, path)
		}
		// Listed, not globbed: a config dir containing '[' would make the
		// directory part of a glob pattern.
		entries, err := os.ReadDir(mcpCacheDir(cfgDir))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			listErr = fmt.Errorf("enumerate MCP OAuth caches: %w", err)
		}
		for _, entry := range entries {
			if name := entry.Name(); strings.HasPrefix(name, "mcp-") && strings.HasSuffix(name, ".json") {
				add(cfgDir, filepath.Join(mcpCacheDir(cfgDir), name))
			}
		}
	}
	if dataDir != "" {
		for _, path := range pluginDataSecrets(dataDir) {
			add(dataDir, path)
		}
	}
	return out, listErr
}

// mcpCacheDir holds one OAuth token cache per remote MCP server.
func mcpCacheDir(cfgDir string) string { return filepath.Join(cfgDir, "mcp") }
