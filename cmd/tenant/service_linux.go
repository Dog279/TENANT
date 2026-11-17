//go:build linux

package main

// service_linux.go (TEN-498): a systemd user unit. The user manager stops
// with the session unless the account lingers, so install checks and prints
// the enable-linger command instead of running it (it needs the operator's
// say). Logs go to the journal; the log book has the rest.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
)

const systemdUnit = `[Unit]
Description=Tenant agent (tenant serve)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart={{q .Exe}} serve --config {{q .CfgDir}} --data {{q .DataDir}} --stop-budget {{secs .StopBudget}}
WorkingDirectory={{.DataDir}}
Restart=on-failure
RestartSec=10
TimeoutStopSec=100

[Install]
WantedBy=default.target
`

var systemdTmpl = template.Must(template.New("unit").Funcs(template.FuncMap{
	// systemd's own quoting for a command line: double quotes, backslash escapes inside.
	"q":    func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` },
	"secs": secs,
}).Parse(systemdUnit))

func (s serviceSpec) unitText() string {
	var b strings.Builder
	_ = systemdTmpl.Execute(&b, s)
	return b.String()
}

func (s serviceSpec) unit() string { return s.Label + ".service" }

// unitDir is where this user's systemd units live. A variable so that the tests keep
// away from this account's real units (service_linux_test.go): `tenant doctor
// --fix` rewrites a unit that does not match the binary and dirs it is run
// with, and under test those are the test binary and temporary dirs.
var unitDir = func() string {
	cfg, _ := os.UserConfigDir() // $XDG_CONFIG_HOME, else ~/.config
	return filepath.Join(cfg, "systemd", "user")
}

func (s serviceSpec) unitPath() string { return filepath.Join(unitDir(), s.unit()) }

func (s serviceSpec) install(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.unitPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.unitPath(), []byte(s.unitText()), 0o644); err != nil {
		return err
	}
	// enable, then restart rather than enable --now: a reinstall with new
	// dirs must apply them to the running service too.
	for _, args := range [][]string{{"daemon-reload"}, {"enable", s.unit()}, {"restart", s.unit()}} {
		if err := s.systemctl(ctx, args...); err != nil {
			return err
		}
	}
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if out, err := runCmd(ctx, "loginctl", "show-user", name, "-p", "Linger", "--value"); err != nil || strings.TrimSpace(out) != "yes" {
		fmt.Printf("note: without lingering the service stops when you log out; to keep it running: loginctl enable-linger %s\n", name)
	}
	return nil
}

func (s serviceSpec) uninstall(ctx context.Context) error {
	if err := s.systemctl(ctx, "disable", "--now", s.unit()); err != nil {
		return err
	}
	if err := os.Remove(s.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.systemctl(ctx, "daemon-reload")
}

func (s serviceSpec) start(ctx context.Context) error   { return s.systemctl(ctx, "start", s.unit()) }
func (s serviceSpec) stop(ctx context.Context) error    { return s.systemctl(ctx, "stop", s.unit()) }
func (s serviceSpec) restart(ctx context.Context) error { return s.systemctl(ctx, "restart", s.unit()) }

func (s serviceSpec) installed(context.Context) (bool, string) {
	_, err := os.Stat(s.unitPath())
	return err == nil, s.unitPath()
}

// unitCurrent: the unit on disk is what install would write now.
func (s serviceSpec) unitCurrent(context.Context) (bool, error) {
	b, err := os.ReadFile(s.unitPath())
	return string(b) == s.unitText(), err
}

func (s serviceSpec) logs() string {
	return "journalctl --user -u " + s.Label + " (the log book has the rest: tenant logs)"
}

func (s serviceSpec) systemctl(ctx context.Context, args ...string) error {
	out, err := runCmd(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return cmdErr("systemctl --user "+args[0], out, err)
	}
	return nil
}

// ownTaskInstalled is Windows' question (service_windows.go): there is no
// `--system` install here to choose against.
func ownTaskInstalled(context.Context) bool { return false }
