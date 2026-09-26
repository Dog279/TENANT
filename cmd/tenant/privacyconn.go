package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	githubp "tenant/internal/plugins/github"
	xp "tenant/internal/plugins/x"
	"tenant/internal/privacy"
)

// connectorDef says where a connected service keeps what Tenant stored for
// it: credentials.json entries under "skill:<name>:" and files of its own.
type connectorDef struct {
	name  string
	files func(cfgDir, dataDir string) []string
}

var connectorDefs = []connectorDef{
	{name: "gsuite"},
	{name: "x", files: func(_, data string) []string { return []string{xp.TokenPath(data)} }},
	{name: "github", files: func(cfg, _ string) []string { return []string{githubp.TokenPath(cfg)} }},
	{name: "atlassian", files: func(cfg, _ string) []string { return []string{atlassianTokenPath(cfg)} }},
	{name: "discord"},
	{name: "imessage"},
	{name: "simplefin", files: func(_, data string) []string { return []string{filepath.Join(data, "simplefin")} }},
	{name: "web"},
	{name: "mcp", files: func(cfg, _ string) []string {
		var out []string
		entries, _ := os.ReadDir(mcpCacheDir(cfg))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "mcp-") && strings.HasSuffix(e.Name(), ".json") {
				out = append(out, filepath.Join(mcpCacheDir(cfg), e.Name()))
			}
		}
		return out
	}},
}

func connectorNames() string {
	var names []string
	for _, d := range connectorDefs {
		names = append(names, d.name)
	}
	return strings.Join(names, ", ")
}

// connectorPlan is what forgetting one connected service removes.
type connectorPlan struct {
	name  string
	files []string // existing files and directories
	keys  []string // credentials.json entries
}

func connectorsEmpty(plans []connectorPlan) bool {
	for _, p := range plans {
		if len(p.files)+len(p.keys) > 0 {
			return false
		}
	}
	return true
}

// planConnector lists what name has stored, without changing anything.
func planConnector(cfgDir, dataDir, name string) (connectorPlan, error) {
	for _, d := range connectorDefs {
		if d.name != name {
			continue
		}
		p := connectorPlan{name: name}
		if d.files != nil {
			for _, f := range d.files(cfgDir, dataDir) {
				if _, err := os.Lstat(f); err == nil {
					p.files = append(p.files, f)
				}
			}
		}
		creds, err := peekCredentials(cfgDir)
		if err != nil {
			return p, err
		}
		prefix := skillSecretID(name, "")
		for k := range creds.Secrets {
			if strings.HasPrefix(k, prefix) {
				p.keys = append(p.keys, k)
			}
		}
		sort.Strings(p.keys)
		return p, nil
	}
	return connectorPlan{}, fmt.Errorf("unknown connector %q; known: %s", name, connectorNames())
}

func (p connectorPlan) print(out io.Writer) {
	if len(p.files)+len(p.keys) == 0 {
		fmt.Fprintf(out, "  %s: nothing stored\n", p.name)
		return
	}
	var what []string
	for _, k := range p.keys {
		what = append(what, "credential "+strings.TrimPrefix(k, skillSecretID(p.name, "")))
	}
	for _, f := range p.files {
		what = append(what, filepath.Base(f))
	}
	fmt.Fprintf(out, "  delete  %6d  %s: %s\n", len(what), p.name, strings.Join(what, ", "))
}

// apply removes the service's files, then its credentials.json entries.
func (p connectorPlan) apply(cfgDir string) error {
	for _, f := range p.files {
		if err := os.RemoveAll(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}
	if len(p.keys) == 0 {
		return nil
	}
	creds, err := loadCredentials(cfgDir)
	if err != nil {
		return err
	}
	for _, k := range p.keys {
		delete(creds.Secrets, k)
	}
	return creds.save(cfgDir)
}

// storedConnectors names each connected service with something stored, and
// what (entry names and file names, never values).
func storedConnectors(cfgDir, dataDir string) []privacy.Connector {
	var out []privacy.Connector
	for _, d := range connectorDefs {
		p, err := planConnector(cfgDir, dataDir, d.name)
		if err != nil || len(p.files)+len(p.keys) == 0 {
			continue
		}
		var stored []string
		for _, k := range p.keys {
			stored = append(stored, "credential "+strings.TrimPrefix(k, skillSecretID(d.name, "")))
		}
		for _, f := range p.files {
			stored = append(stored, filepath.Base(f))
		}
		out = append(out, privacy.Connector{Name: d.name, Stored: stored})
	}
	return out
}
