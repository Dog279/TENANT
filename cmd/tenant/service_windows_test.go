//go:build windows

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func windowsFixture(system bool) serviceSpec {
	s := serviceSpec{Exe: `C:\Program Files\Tenant\tenant.exe`, CfgDir: `C:\Users\Jane Doe\AppData\Roaming\tenant`,
		DataDir: `C:\Users\Jane Doe\AppData\Local\tenant`, StopBudget: 4 * time.Second, Label: `\Tenant\Serve`}
	if system {
		s.System, s.StopBudget, s.Label = true, 120*time.Second, "Tenant"
	}
	return s
}

const userTR = `"C:\Program Files\Tenant\tenant.exe" serve --config "C:\Users\Jane Doe\AppData\Roaming\tenant" --data "C:\Users\Jane Doe\AppData\Local\tenant" --stop-budget 4s --hidden`
const systemCmd = `"C:\Program Files\Tenant\tenant.exe" serve --config "C:\Users\Jane Doe\AppData\Roaming\tenant" --data "C:\Users\Jane Doe\AppData\Local\tenant" --stop-budget 120s`

// What --print shows: the task or service, and the command line it runs.
func TestUnitText(t *testing.T) {
	if got, want := windowsFixture(false).unitText(), "Scheduled task \\Tenant\\Serve (at logon, as the current user)\n  "+userTR+"\n"; got != want {
		t.Errorf("user unit:\n%s\nwant:\n%s", got, want)
	}
	if got, want := windowsFixture(true).unitText(), "Windows service Tenant (automatic start, runs as SYSTEM)\n  "+systemCmd+"\n"; got != want {
		t.Errorf("system unit:\n%s\nwant:\n%s", got, want)
	}
}

func elevated(t *testing.T, yes bool) {
	t.Helper()
	prev := isElevated
	isElevated = func() bool { return yes }
	t.Cleanup(func() { isElevated = prev })
}

func TestInstallArgv(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	if err := windowsFixture(false).install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, `schtasks /Create /F /TN \Tenant\Serve /SC ONLOGON /RL LIMITED /TR `+userTR, `schtasks /Run /TN \Tenant\Serve`)

	rec.calls = nil
	elevated(t, true)
	if err := windowsFixture(true).install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "sc create Tenant binPath= "+systemCmd+" start= auto DisplayName= Tenant",
		"sc description Tenant Tenant agent (tenant serve)",
		"sc failure Tenant reset= 86400 actions= restart/10000/restart/60000/restart/300000",
		"sc start Tenant")

	rec.calls = nil
	elevated(t, false)
	if err := windowsFixture(true).install(context.Background()); err == nil || !strings.Contains(err.Error(), "elevated") {
		t.Errorf("--system without elevation: err = %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("called sc without elevation: %v", rec.calls)
	}
}

func TestUninstallArgv(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	if err := windowsFixture(false).uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, `schtasks /End /TN \Tenant\Serve`, `schtasks /Delete /F /TN \Tenant\Serve`)

	rec.calls = nil
	rec.reply = func(argv []string) (string, error) {
		if argv[1] == "query" {
			return "        STATE              : 1  STOPPED\n", nil
		}
		return "", nil
	}
	if err := windowsFixture(true).uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "sc stop Tenant", "sc query Tenant", "sc delete Tenant")
}

func TestRestartArgv(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	if err := windowsFixture(false).restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, `schtasks /End /TN \Tenant\Serve`, `schtasks /Run /TN \Tenant\Serve`)

	rec.calls = nil
	queries := 0
	rec.reply = func(argv []string) (string, error) {
		if argv[1] == "query" { // running once, then stopped
			if queries++; queries == 1 {
				return "        STATE              : 4  RUNNING\n", nil
			}
			return "        STATE              : 1  STOPPED\n", nil
		}
		return "", nil
	}
	if err := windowsFixture(true).restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "sc stop Tenant", "sc query Tenant", "sc query Tenant", "sc start Tenant")
}

// 261 characters of /TR go through; 262 are refused before schtasks runs.
func TestInstallRefusesLongTR(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	spec := windowsFixture(false)
	spec.DataDir = ""
	spec.DataDir = strings.Repeat("x", maxTR-len(spec.cmdLine()))
	if n := len(spec.cmdLine()); n != maxTR {
		t.Fatalf("fixture is %d characters", n)
	}
	if err := spec.install(context.Background()); err != nil || len(rec.calls) != 2 {
		t.Errorf("261: err = %v, %d calls", err, len(rec.calls))
	}
	rec.calls = nil
	spec.DataDir += "x"
	if err := spec.install(context.Background()); err == nil || !strings.Contains(err.Error(), "shorten --config/--data") {
		t.Errorf("262: err = %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("262 still ran schtasks: %v", rec.calls)
	}
}

// TestServiceLive registers a real logon task under a test name that runs
// this test binary (which exits at once on serve's flags), then removes it;
// from an elevated terminal, the --system service too.
func TestServiceLive(t *testing.T) {
	if os.Getenv("TENANT_LIVE_SERVICE") != "1" {
		t.Skip("TENANT_LIVE_SERVICE=1 runs this against the Task Scheduler and the SCM")
	}
	exe, _ := os.Executable()
	ctx := context.Background()
	modes := []bool{false}
	if isElevated() {
		modes = append(modes, true)
	}
	for _, system := range modes {
		spec, err := specFor("windows", exe, t.TempDir(), t.TempDir(), system)
		if err != nil {
			t.Fatal(err)
		}
		if system {
			spec.Label = "TenantTest"
		} else {
			spec.Label = `\Tenant\Test`
		}
		if err := spec.install(ctx); err != nil {
			t.Fatalf("system=%v: %v", system, err)
		}
		if ok, where := spec.installed(ctx); !ok {
			t.Errorf("system=%v: installed() = false, %s", system, where)
		}
		if err := spec.uninstall(ctx); err != nil {
			t.Fatalf("system=%v uninstall: %v", system, err)
		}
		if ok, _ := spec.installed(ctx); ok {
			t.Errorf("system=%v: installed() after uninstall", system)
		}
	}
}
