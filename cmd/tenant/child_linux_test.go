//go:build linux

package main

import (
	"context"
	"os"
	"testing"
)

// A child's own unit: an account's, or with --system the machine's, started
// at boot as root. Both run `tenant child run` on the child's directory.
func TestChildUnitLinux(t *testing.T) {
	user, err := childSpecFor("linux", "/home/jane doe/.local/bin/tenant", "/home/jane doe/.config/tenant/child", false)
	if err != nil {
		t.Fatal(err)
	}
	want := `[Unit]
Description=Tenant fleet child (tenant child run)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart="/home/jane doe/.local/bin/tenant" child run --dir "/home/jane doe/.config/tenant/child"
WorkingDirectory=/home/jane doe/.config/tenant/child-data
Restart=on-failure
RestartSec=10
TimeoutStopSec=100

[Install]
WantedBy=default.target
`
	if got := user.unitText(); got != want {
		t.Errorf("user unit:\n%s\nwant:\n%s", got, want)
	}
	sys, err := childSpecFor("linux", "/usr/local/bin/tenant", "/etc/tenant/child", true)
	if err != nil {
		t.Fatal(err)
	}
	want = `[Unit]
Description=Tenant fleet child (tenant child run)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart="/usr/local/bin/tenant" child run --dir "/etc/tenant/child"
WorkingDirectory=/etc/tenant/child-data
Restart=on-failure
RestartSec=10
TimeoutStopSec=100

[Install]
WantedBy=multi-user.target
`
	if got := sys.unitText(); got != want {
		t.Errorf("system unit:\n%s\nwant:\n%s", got, want)
	}
	if sys.logs() != "journalctl -u tenant-child" || user.unit() != "tenant-child.service" {
		t.Errorf("logs %q, unit %q", sys.logs(), user.unit())
	}

	childServiceSeams(t)
	rec := &cmdRecorder{}
	rec.install(t)
	if err := sys.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "systemctl daemon-reload", "systemctl enable tenant-child.service", "systemctl restart tenant-child.service")
	if fi, err := os.Stat(sys.unitPath()); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the unit file: %v, %v", fi, err)
	}
	rec.calls = nil
	if err := sys.uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "systemctl disable --now tenant-child.service", "systemctl daemon-reload")
	if exists(sys.unitPath()) {
		t.Error("uninstall left the unit file")
	}
	rec.calls = nil
	if err := user.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "systemctl --user daemon-reload", "systemctl --user enable tenant-child.service", "systemctl --user restart tenant-child.service",
		"loginctl show-user "+userName()+" -p Linger --value")
}
