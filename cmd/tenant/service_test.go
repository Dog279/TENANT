package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// No test reaches this machine's service manager unless it asks for it
// (liveServiceManager): TestMain puts noServiceManager in runCmd's place.
// Found on 2026-11-17 on a machine with Tenant installed as a logon task: the
// tests that run `tenant doctor --fix` saw the task, found it did not match
// the test binary and its temporary dirs, and rewrote it, so the developer's
// own task then started the test binary. With no manager to ask, doctor sees
// no service installed.
var realRunCmd = runCmd

var errNoServiceManager = errors.New("no service manager under test")

func noServiceManager(context.Context, string, ...string) (string, error) {
	return "", errNoServiceManager
}

// liveServiceManager gives a test the real launchctl, systemctl, schtasks
// and sc: the TENANT_LIVE_SERVICE tests, which install a unit of their own.
func liveServiceManager(t *testing.T) {
	t.Helper()
	prev := runCmd
	runCmd = realRunCmd
	t.Cleanup(func() { runCmd = prev })
}

func TestNoTestReachesTheServiceManagerByDefault(t *testing.T) {
	if _, err := runCmd(context.Background(), "schtasks", "/Query", "/TN", `\Tenant\Serve`); !errors.Is(err, errNoServiceManager) {
		t.Fatalf("runCmd under test = %v, want the refusal: a test could rewrite this machine's own service", err)
	}
	if systemInstalled(context.Background()) {
		t.Error("a --system install was found without asking a service manager")
	}
}

// cmdRecorder swaps runCmd: argv in order, and a reply per command.
type cmdRecorder struct {
	calls [][]string
	reply func(argv []string) (string, error)
}

func (r *cmdRecorder) install(t *testing.T) {
	t.Helper()
	prev := runCmd
	runCmd = func(_ context.Context, name string, args ...string) (string, error) {
		argv := append([]string{name}, args...)
		r.calls = append(r.calls, argv)
		if r.reply != nil {
			return r.reply(argv)
		}
		return "", nil
	}
	t.Cleanup(func() { runCmd = prev })
}

func (r *cmdRecorder) want(t *testing.T, want ...string) {
	t.Helper()
	var got []string
	for _, c := range r.calls {
		got = append(got, strings.Join(c, " "))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The label, stop budget and log location per OS and mode; ~ expanded and
// dirs absolute; the symlinked binary resolved.
func TestServiceSpec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cwd, _ := os.Getwd()
	for _, tc := range []struct {
		goos   string
		system bool
		label  string
		budget time.Duration
		logDir string
		err    string
	}{
		{"darwin", false, "com.tenant.serve", 90 * time.Second, filepath.Join(home, "Library", "Logs", "tenant"), ""},
		{"darwin", true, "", 0, "", "not supported yet on macOS"},
		{"linux", false, "tenant", 90 * time.Second, "", ""},
		{"linux", true, "", 0, "", "not supported yet on Linux"},
		{"windows", false, `\Tenant\Serve`, 4 * time.Second, "", ""},
		{"windows", true, "Tenant", 120 * time.Second, "", ""},
	} {
		s, err := specFor(tc.goos, "/opt/tenant/tenant", "~/cfg", "data", tc.system)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s system=%v: err = %v, want %q", tc.goos, tc.system, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.goos, err)
		}
		if s.CfgDir != filepath.Join(home, "cfg") || s.DataDir != filepath.Join(cwd, "data") {
			t.Errorf("%s: dirs = %q %q, want ~ expanded and absolute", tc.goos, s.CfgDir, s.DataDir)
		}
		if s.Label != tc.label || s.StopBudget != tc.budget || s.LogDir != tc.logDir || s.System != tc.system {
			t.Errorf("%s system=%v: %+v", tc.goos, tc.system, s)
		}
	}

	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	want, _ := filepath.EvalSymlinks(real) // the temp dir may itself be a link (/var → /private/var)
	if got, err := resolveExe(link); err != nil || got != want {
		t.Errorf("resolveExe(link) = %q, %v; want %q", got, err, want)
	}
	exe, _ := os.Executable()
	want, _ = resolveExe(exe)
	if s, err := newServiceSpec(&commonFlags{cfgDir: home, dataDir: home}, false); err != nil || s.Exe != want {
		t.Errorf("newServiceSpec: exe = %q, %v; want this binary %q", s.Exe, err, want)
	}
}

// Desktop, Documents and Downloads are refused, by name; Library and a
// sibling with a longer name are not.
func TestServiceRefusesTCCDirs(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "jane")
	for _, tc := range []struct {
		path    string
		refused bool
	}{
		{filepath.Join(home, "Desktop", "tenant"), true},
		{filepath.Join(home, "Documents", "tenant", "data"), true},
		{filepath.Join(home, "Downloads"), true},
		{filepath.Join(home, "Desktopia", "tenant"), false},
		{filepath.Join(home, "Library", "Application Support", "tenant"), false},
		{filepath.Join(home, ".local", "bin", "tenant"), false},
		{filepath.Join(string(filepath.Separator), "opt", "tenant"), false},
	} {
		err := tccRefused(home, tc.path)
		if (err != nil) != tc.refused {
			t.Errorf("tccRefused(%s) = %v, want refused=%v", tc.path, err, tc.refused)
		}
		if err != nil && !strings.Contains(err.Error(), tc.path) {
			t.Errorf("the error must name the path: %v", err)
		}
	}
}

// restart waits for turn_active to go false before touching the manager;
// --now does not look; a hub that stays busy past the timeout is "still
// busy"; a hub that does not answer has nothing to drain.
func TestWaitDrained(t *testing.T) {
	prev := drainPoll
	drainPoll = 2 * time.Millisecond
	t.Cleanup(func() { drainPoll = prev })

	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"turn_active":%v}`, polls.Add(1) < 3)
	}))
	defer srv.Close()
	rec := &cmdRecorder{}
	rec.install(t)
	firedAfter := int32(-1)
	rec.reply = func([]string) (string, error) {
		if firedAfter < 0 {
			firedAfter = polls.Load()
		}
		return "", nil
	}
	spec, err := specFor(runtime.GOOS, filepath.Join(t.TempDir(), "tenant"), t.TempDir(), t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := serviceRestart(ctx, spec, srv.URL, "", false); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if firedAfter != 3 || len(rec.calls) == 0 {
		t.Errorf("the manager was called after %d polls (%d calls), want after the third", firedAfter, len(rec.calls))
	}

	polls.Store(0)
	rec.calls, firedAfter = nil, -1
	if err := serviceRestart(ctx, spec, srv.URL, "", true); err != nil {
		t.Fatalf("restart --now: %v", err)
	}
	if polls.Load() != 0 || len(rec.calls) == 0 {
		t.Errorf("--now: %d polls, %d calls; want none and some", polls.Load(), len(rec.calls))
	}

	polls.Store(-1000) // busy for a long time
	if err := waitDrained(ctx, srv.URL, "", 0); err == nil || err.Error() != "still busy; use --now" {
		t.Errorf("timeout: err = %v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if err := waitDrained(ctx, gone.URL, "", time.Second); err != nil {
		t.Errorf("unreachable hub: err = %v, want nil", err)
	}
}

// The power-cut finding (Windows Server 2025, 2026-11-17): config.json came
// back unreadable, serve refused to start on it, and `tenant service
// uninstall` refused too, so the broken service could not be removed.
// Stopping and removing the unit need nothing from the config; the verbs
// that do still say what is wrong with it.
func TestServiceUninstallAndStopWorkWithoutAConfig(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(k, home) // a unit path under here, never the developer's own
	}
	cfg, data := filepath.Join(home, "cfg"), filepath.Join(home, "data")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	// What the power cut left: the right length, every byte zero.
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(verb string) (*cmdRecorder, error) {
		rec := &cmdRecorder{reply: func(argv []string) (string, error) {
			// No --system service (Windows) and nothing loaded (macOS), so
			// neither waits for a stop that the recorder never performs.
			if argv[0] == "sc" || argv[0] == "launchctl" && argv[1] == "print" {
				return "", errors.New("exit status 1")
			}
			return "", nil
		}}
		rec.install(t)
		return rec, cmdService(context.Background(), []string{verb, "--config", cfg, "--data", data})
	}
	for _, verb := range []string{"uninstall", "stop"} {
		rec, err := run(verb)
		if err != nil {
			t.Errorf("service %s with a corrupt config: %v", verb, err)
		}
		if len(rec.calls) == 0 {
			t.Errorf("service %s reached no service manager", verb)
		}
	}
	for _, verb := range []string{"install", "start", "restart", "status"} {
		rec, err := run(verb)
		if err == nil || !strings.Contains(err.Error(), "config.json corrupt") {
			t.Errorf("service %s with a corrupt config = %v, want the config error", verb, err)
		}
		if len(rec.calls) != 0 {
			t.Errorf("service %s ran %v before reading the config", verb, rec.calls)
		}
	}
}
