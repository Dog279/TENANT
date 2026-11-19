//go:build darwin

package main

import (
	"context"
	"strings"
	"testing"
)

// A child's own unit: a LaunchAgent of the account that installed it, or
// with --system a LaunchDaemon the machine starts at boot as root. Both run
// `tenant child run` on the child's directory.
func TestChildUnitDarwin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := "/Library/Application Support/tenant/child"
	sys, err := childSpecFor("darwin", "/usr/local/bin/tenant", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.tenant.child</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/tenant</string>
    <string>child</string>
    <string>run</string>
    <string>--dir</string>
    <string>/Library/Application Support/tenant/child</string>
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
  <string>/Library/Application Support/tenant/child-data</string>
  <key>StandardOutPath</key>
  <string>/Library/Application Support/tenant/child-data/logs/child.log</string>
  <key>StandardErrorPath</key>
  <string>/Library/Application Support/tenant/child-data/logs/child.err.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>/var/root</string>
  </dict>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`
	if got := sys.unitText(); got != want {
		t.Errorf("the LaunchDaemon:\n%s\nwant:\n%s", got, want)
	}
	if sys.domain() != "system" || sys.target() != "system/com.tenant.child" || !strings.HasSuffix(sys.logs(), "child.log and child.err.log") {
		t.Errorf("domain %q, target %q, logs %q", sys.domain(), sys.target(), sys.logs())
	}
	user, err := childSpecFor("darwin", "/usr/local/bin/tenant", home+"/.config/tenant/child", false)
	if err != nil {
		t.Fatal(err)
	}
	text := user.unitText()
	if !strings.Contains(text, "<string>"+home+"</string>") || !strings.Contains(text, "<string>"+home+"/.config/tenant/child</string>") ||
		!strings.HasPrefix(user.domain(), "gui/") {
		t.Errorf("the LaunchAgent, in domain %s:\n%s", user.domain(), text)
	}

	childServiceSeams(t)
	rec := &cmdRecorder{reply: func(argv []string) (string, error) {
		if argv[1] == "print" {
			return "", context.Canceled // not loaded
		}
		return "", nil
	}}
	rec.install(t)
	// The daemon's directories must be makeable here: point it at a temp tree.
	tmp, err := childSpecFor("darwin", "/usr/local/bin/tenant", t.TempDir()+"/child", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "launchctl print system/com.tenant.child", "launchctl bootstrap system "+tmp.unitPath())
	if !strings.HasPrefix(tmp.unitPath(), systemUnitDir()) || !strings.HasSuffix(tmp.unitPath(), "com.tenant.child.plist") {
		t.Errorf("the daemon's plist is at %s", tmp.unitPath())
	}
}
