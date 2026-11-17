package main

// service.go (TEN-498): `tenant service` runs serve under the OS's own
// supervisor, so a newcomer gets a background Tenant that survives logout
// and reboot without hand-editing a unit: a LaunchAgent on macOS, a systemd
// user unit on Linux, a logon scheduled task on Windows (or, with --system,
// a real SCM service for a headless box). Each OS file (service_<os>.go)
// owns its unit text and the calls to its manager; this file is the
// dispatch, the spec they share, the one seam every external command goes
// through, and the drain before a restart. docs/plans/install-upgrade-plan.md
// (PR A) is the design; PR E adds what serve does with the stop budget.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// serviceSpec is everything a unit needs. Paths are absolute: a supervisor
// has no shell and no working directory of ours to resolve them against.
type serviceSpec struct {
	Exe, CfgDir, DataDir, LogDir string
	System                       bool          // Windows only: an SCM service as SYSTEM; elsewhere "not supported yet"
	StopBudget                   time.Duration // passed as --stop-budget: the grace the OS grants a stop (the plan's PR E table)
	Label                        string        // the unit's name under its manager
}

// runCmd runs one service-manager command and returns its combined output.
// The only way out to launchctl, systemctl, loginctl, schtasks and sc; tests
// swap in a recorder, the way holdDataDir and promptPassphrase are swapped.
var runCmd = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// drainPoll is how often restart re-reads /api/status while a turn runs;
// drainTimeout is a cron run's ceiling, after which restart gives up.
var drainPoll = 5 * time.Second

const drainTimeout = 15 * time.Minute

const serviceUsage = "usage: tenant service install [--print] [--system] | uninstall | status | start | stop | restart [--now]"

func cmdService(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(serviceUsage)
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet("service "+verb, flag.ContinueOnError)
	c := bindCommon(fs)
	print := fs.Bool("print", false, "install: print the unit instead of installing it")
	system := fs.Bool("system", false, "Windows: a service that runs as SYSTEM (headless boxes; needs an elevated terminal)")
	now := fs.Bool("now", false, "restart: don't wait for the running turn to finish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	// The config gives the dashboard address and token, which restart and
	// status ask for. Stopping and removing the unit need neither, and a
	// config.json that no longer loads is when the operator most needs them:
	// serve will not start on it, and the unit keeps trying.
	if err := c.resolveConfig(); err != nil {
		if !serviceWorksWithoutConfig(verb) {
			return err
		}
		fmt.Printf("warning: %v\n(%s does not need the config; carrying on)\n", err, verb)
	}
	// Without --system, the verbs that act on what is there act on a
	// --system install when that is what is installed (Windows).
	spec, err := newServiceSpec(c, *system || verb != "install" && systemInstalled(ctx))
	if err != nil {
		return err
	}
	base := attachBaseURL("", c.lc)
	token, _ := dashboardClientToken(c.cfgDir, c.lc, true) // "" when none yet; the hub then just isn't reachable
	switch verb {
	case "install":
		if *print {
			fmt.Print(spec.unitText())
			return nil
		}
		return installService(ctx, spec)
	case "uninstall":
		return spec.uninstall(ctx)
	case "start":
		return spec.start(ctx)
	case "stop":
		return spec.stop(ctx)
	case "restart":
		return serviceRestart(ctx, spec, base, token, *now)
	case "status":
		return serviceStatus(ctx, os.Stdout, spec, base, token)
	}
	return fmt.Errorf("unknown service command %q\n%s", verb, serviceUsage)
}

// serviceWorksWithoutConfig names the verbs that act on the unit alone.
func serviceWorksWithoutConfig(verb string) bool { return verb == "uninstall" || verb == "stop" }

// newServiceSpec is the unit for this binary and this command's dirs.
func newServiceSpec(c *commonFlags, system bool) (serviceSpec, error) {
	exe, err := os.Executable()
	if err != nil {
		return serviceSpec{}, err
	}
	if exe, err = resolveExe(exe); err != nil {
		return serviceSpec{}, err
	}
	return specFor(runtime.GOOS, exe, c.cfgDir, c.dataDir, system)
}

// resolveExe is the binary the unit will run: symlinks resolved, so the
// file an upgrade replaces (PR B) is the file the supervisor starts.
func resolveExe(p string) (string, error) {
	p, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// specFor is newServiceSpec's pure half: the label, stop budget and log
// location each OS and mode gets, so one test covers every row.
func specFor(goos, exe, cfgDir, dataDir string, system bool) (serviceSpec, error) {
	cfg, err := filepath.Abs(expandPath(cfgDir))
	if err != nil {
		return serviceSpec{}, err
	}
	data, err := filepath.Abs(expandPath(dataDir))
	if err != nil {
		return serviceSpec{}, err
	}
	s := serviceSpec{Exe: exe, CfgDir: cfg, DataDir: data, System: system, StopBudget: 90 * time.Second, Label: "tenant"}
	switch goos {
	case "darwin":
		if system {
			return s, errors.New("--system is not supported yet on macOS (a LaunchDaemon is a later ticket)")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return s, err
		}
		s.Label, s.LogDir = "com.tenant.serve", filepath.Join(home, "Library", "Logs", "tenant")
	case "windows":
		// 4 s: a logoff gives a window about five (sessionend_windows.go).
		s.Label, s.StopBudget = `\Tenant\Serve`, 4*time.Second
		if system {
			s.Label, s.StopBudget = "Tenant", 120*time.Second
		}
	default:
		if system {
			return s, errors.New("--system is not supported yet on Linux (a system unit is a later ticket)")
		}
	}
	return s, nil
}

// systemInstalled reports a Windows `--system` install, so status, stop,
// restart and uninstall act on it without the flag. False elsewhere.
func systemInstalled(ctx context.Context) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	_, err := runCmd(ctx, "sc", "query", "Tenant") // the --system label (specFor)
	return err == nil
}

// installService installs and starts the unit and says where it went; setup
// calls it after a successful interactive run.
func installService(ctx context.Context, spec serviceSpec) error {
	if err := spec.install(ctx); err != nil {
		return err
	}
	_, where := spec.installed(ctx)
	fmt.Printf("installed %s; `tenant service status` shows it\n", where)
	return nil
}

// serviceRestart drains first: a restart mid-turn throws the turn away, and
// a cron run can take minutes. --now skips the wait.
func serviceRestart(ctx context.Context, spec serviceSpec, base, token string, now bool) error {
	if !now {
		if err := waitDrained(ctx, base, token, drainTimeout); err != nil {
			return err
		}
	}
	return spec.restart(ctx)
}

// waitDrained polls /api/status until no turn is active. A hub that does
// not answer has nothing to drain, so that returns at once.
func waitDrained(ctx context.Context, base, token string, timeout time.Duration) error {
	httpc := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(timeout)
	told := false
	for {
		st, err := attachFetchStatus(ctx, httpc, base, token)
		if err != nil || !st.TurnActive {
			return nil
		}
		if !told {
			fmt.Println("a turn is running; waiting for it to finish (--now skips the wait)")
			told = true
		}
		if !time.Now().Before(deadline) {
			return errors.New("still busy; use --now")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPoll):
		}
	}
}

func serviceStatus(ctx context.Context, w io.Writer, spec serviceSpec, base, token string) error {
	if ok, where := spec.installed(ctx); ok {
		fmt.Fprintf(w, "installed: yes (%s)\n", where)
	} else {
		fmt.Fprintln(w, "installed: no (tenant service install)")
	}
	st, err := attachFetchStatus(ctx, &http.Client{Timeout: 5 * time.Second}, base, token)
	if err != nil {
		fmt.Fprintf(w, "running:   not reachable at %s (%v)\n", base, err)
	} else {
		fmt.Fprintf(w, "running:   version %s, turn_active=%v\n", firstNonEmpty(st.Version, "unknown"), st.TurnActive)
	}
	fmt.Fprintf(w, "logs:      %s\n", spec.logs())
	return nil
}

// tccRefused is the macOS rule: a launchd-spawned process is silently
// denied files under Desktop, Documents and Downloads (TCC), so a unit must
// not point into them. Pure, with home injected, so the rule is tested on
// every OS.
func tccRefused(home string, paths ...string) error {
	for _, p := range paths {
		for _, d := range []string{"Desktop", "Documents", "Downloads"} {
			dir := filepath.Join(home, d)
			if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
				return fmt.Errorf("%s is under ~/%s, which macOS denies to background services (TCC); move it "+
					"(good homes: ~/.local/bin for the binary, ~/.config/tenant for config, ~/Library/Application Support/tenant for data)", p, d)
			}
		}
	}
	return nil
}

// secs renders a budget the way the units pass it: "90s", not "1m30s".
func secs(d time.Duration) string { return fmt.Sprintf("%ds", int(d/time.Second)) }

// cmdErr is a failed manager command with what it said.
func cmdErr(what, out string, err error) error {
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("%s: %s", what, s)
	}
	return fmt.Errorf("%s: %w", what, err)
}
