//go:build darwin

package main

// service_darwin.go (TEN-498): a per-user LaunchAgent, the keys the
// hand-edited template carried (deploy/launchd, now gone) plus ExitTimeOut:
// launchd's default grace is system-defined (man launchd.plist) and serve's
// teardown gets 90. launchd keeps the plist it loaded, so a reinstall unloads
// first; a stop unloads and keeps the file, so start can load it again.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{xml .Exe}}</string>
    <string>serve</string>
    <string>--config</string>
    <string>{{xml .CfgDir}}</string>
    <string>--data</string>
    <string>{{xml .DataDir}}</string>
    <string>--stop-budget</string>
    <string>{{secs .StopBudget}}</string>
  </array>
  <key>KeepAlive</key>
  <dict>
    <key>Crashed</key>
    <true/>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ExitTimeOut</key>
  <integer>100</integer>
  <key>WorkingDirectory</key>
  <string>{{xml .DataDir}}</string>
  <key>StandardOutPath</key>
  <string>{{xml .LogDir}}/serve.log</string>
  <key>StandardErrorPath</key>
  <string>{{xml .LogDir}}/serve.err.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>{{xml .Home}}</string>
  </dict>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`

var launchdTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{
	"xml":  strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace,
	"secs": secs,
}).Parse(launchdPlist))

func (s serviceSpec) unitText() string {
	home, _ := os.UserHomeDir()
	var b strings.Builder
	_ = launchdTmpl.Execute(&b, struct {
		serviceSpec
		Home string
	}{s, home})
	return b.String()
}

func (s serviceSpec) unitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", s.Label+".plist")
}

func (s serviceSpec) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }
func (s serviceSpec) target() string { return s.domain() + "/" + s.Label }

func (s serviceSpec) install(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := tccRefused(home, s.Exe, s.CfgDir, s.DataDir); err != nil {
		return err
	}
	for _, d := range []string{s.LogDir, filepath.Dir(s.unitPath())} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(s.unitPath(), []byte(s.unitText()), 0o644); err != nil {
		return err
	}
	if err := s.unload(ctx); err != nil {
		return err
	}
	return s.bootstrap(ctx) // RunAtLoad starts it
}

func (s serviceSpec) uninstall(ctx context.Context) error {
	if err := s.unload(ctx); err != nil {
		return err
	}
	if err := os.Remove(s.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s serviceSpec) start(ctx context.Context) error {
	if !s.loaded(ctx) {
		return s.bootstrap(ctx)
	}
	out, err := runCmd(ctx, "launchctl", "kickstart", s.target()) // loaded but exited: start it
	if err != nil {
		return cmdErr("launchctl kickstart", out, err)
	}
	return nil
}

func (s serviceSpec) stop(ctx context.Context) error { return s.unload(ctx) }

func (s serviceSpec) restart(ctx context.Context) error {
	if !s.loaded(ctx) {
		return s.bootstrap(ctx)
	}
	out, err := runCmd(ctx, "launchctl", "kickstart", "-k", s.target()) // SIGTERM, then start
	if err != nil {
		return cmdErr("launchctl kickstart -k", out, err)
	}
	return nil
}

func (s serviceSpec) installed(context.Context) (bool, string) {
	_, err := os.Stat(s.unitPath())
	return err == nil, s.unitPath()
}

// unitCurrent: the plist on disk is what install would write now.
func (s serviceSpec) unitCurrent(context.Context) (bool, error) {
	b, err := os.ReadFile(s.unitPath())
	return string(b) == s.unitText(), err
}

func (s serviceSpec) logs() string {
	return filepath.Join(s.LogDir, "serve.log") + " and serve.err.log (the log book has the rest: tenant logs)"
}

// loaded: launchd knows the label in this user's domain.
func (s serviceSpec) loaded(ctx context.Context) bool {
	_, err := runCmd(ctx, "launchctl", "print", s.target())
	return err == nil
}

func (s serviceSpec) bootstrap(ctx context.Context) error {
	out, err := runCmd(ctx, "launchctl", "bootstrap", s.domain(), s.unitPath())
	if err != nil {
		return cmdErr("launchctl bootstrap", out, err)
	}
	return nil
}

// unload boots the job out and waits until launchd has let go of it: the
// stop is graceful (SIGTERM, up to the budget), and a bootstrap right after
// an unfinished bootout fails. Not loaded is fine.
func (s serviceSpec) unload(ctx context.Context) error {
	if !s.loaded(ctx) {
		return nil
	}
	if out, err := runCmd(ctx, "launchctl", "bootout", s.target()); err != nil {
		return cmdErr("launchctl bootout", out, err)
	}
	deadline := time.Now().Add(s.StopBudget + 10*time.Second)
	for s.loaded(ctx) {
		if time.Now().After(deadline) {
			return errors.New("launchctl bootout: the service is still loaded after the stop budget")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}
