package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/sandbox"
	"tenant/internal/sandbox/escapecore"
)

// probe lists every surface and reports the host backend honestly: no
// filesystem or network enforcement. Tolerates a surface whose backend is
// unavailable on this host (probe then exits non-zero by design).
func TestSandboxProbe_ListsEverySurfaceAndHonestHost(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	dirs := []string{"--config", cfg, "--data", data}
	ctx := context.Background()

	var human bytes.Buffer
	if err := sandboxMain(ctx, append([]string{"probe"}, dirs...), &human); err != nil && !errors.Is(err, errSurfaceUnavailable) {
		t.Fatalf("probe: %v", err)
	}
	out := human.String()
	for _, s := range sandboxSurfaces {
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(s) + `\s+\S+\s+\S+`).MatchString(out) {
			t.Errorf("probe table has no row for surface %q:\n%s", s, out)
		}
	}
	if !regexp.MustCompile(`(?m)^\s+host\s+available\s+none\s+none\s+none\s`).MatchString(out) {
		t.Errorf("host backend row must show isolation, filesystem and network as none:\n%s", out)
	}

	var js bytes.Buffer
	if err := sandboxMain(ctx, append([]string{"probe", "--json"}, dirs...), &js); err != nil && !errors.Is(err, errSurfaceUnavailable) {
		t.Fatalf("probe --json: %v", err)
	}
	var rep probeReport
	if err := json.Unmarshal(js.Bytes(), &rep); err != nil {
		t.Fatalf("probe --json is not JSON: %v\n%s", err, js.String())
	}
	var names []string
	for _, s := range rep.Surfaces {
		names = append(names, s.Surface)
		if !slices.Contains(s.DenyRoots, cfg) || !slices.Contains(s.DenyRoots, data) {
			t.Errorf("%s: deny roots %v must pin the config and data dirs", s.Surface, s.DenyRoots)
		}
		if s.Backend == "host" && (s.Caps == nil || s.Caps.Filesystem != "none" || s.Caps.Network != "none") {
			t.Errorf("%s runs on host, which must report fs/network none: %+v", s.Surface, s.Caps)
		}
	}
	if !slices.Equal(names, sandboxSurfaces) {
		t.Errorf("surfaces = %v, want %v", names, sandboxSurfaces)
	}
	if h := rep.Host; h.Isolation != "none" || h.Filesystem != "none" || h.Network != "none" {
		t.Errorf("host capabilities must be honest (isolation/fs/net none): %+v", h)
	}
	if runtime.GOOS == "darwin" {
		if r := rep.Restricted; !r.Available || r.Caps == nil || r.Caps.Filesystem != "enforced" {
			t.Errorf("darwin must report the Seatbelt restricted backend even when no surface uses it: %+v", r)
		}
	}
}

// decideSelftest is the whole pass/fail policy; every rule is pinned here.
func TestSandboxSelftestDecision(t *testing.T) {
	full := sandbox.Capabilities{Backend: "seatbelt", Isolation: sandbox.IsolationProcess,
		Filesystem: sandbox.EnforceFull, Network: sandbox.EnforceFull, Limits: sandbox.EnforcePartial, Env: sandbox.EnforceFull}
	noNet := full
	noNet.Network = sandbox.EnforceNone
	partialAll := sandbox.Capabilities{Backend: "restricted", Isolation: sandbox.IsolationProcess,
		Filesystem: sandbox.EnforcePartial, Network: sandbox.EnforcePartial, Limits: sandbox.EnforcePartial, Env: sandbox.EnforcePartial}

	mk := func(name, kind string, contained, legitOK bool) escapecore.CaseResult {
		r := escapecore.CaseResult{Name: name, Kind: kind, AdversarialContained: contained, LegitOK: legitOK}
		if contained {
			r.AdversarialExit = 1
		}
		if !legitOK {
			r.LegitExit = 1
		}
		return r
	}
	fsCase := func(name string, contained, legitOK bool) escapecore.CaseResult {
		return mk(name, escapecore.KindFilesystem, contained, legitOK)
	}
	netCase := func(contained, legitOK bool) escapecore.CaseResult {
		return mk("net", escapecore.KindNetwork, contained, legitOK)
	}
	skippedNet := escapecore.CaseResult{Name: "net", Kind: escapecore.KindNetwork, Skipped: true, SkipReason: "no network", LegitOK: true}

	host := func(cases ...escapecore.CaseResult) selftestRun {
		return selftestRun{Backend: "host", Caps: sandbox.Host().Probe(context.Background()), Cases: cases}
	}
	restricted := func(caps sandbox.Capabilities, cases ...escapecore.CaseResult) *selftestRun {
		return &selftestRun{Backend: "restricted", Caps: caps, Cases: cases}
	}
	baseline := host(fsCase("read", false, true), fsCase("write", false, true), netCase(false, true))
	allContained := restricted(full, fsCase("read", true, true), fsCase("write", true, true), netCase(true, true))

	tests := []struct {
		name         string
		host         selftestRun
		restricted   *selftestRun
		unavailable  string
		wantPass     bool
		wantProblem  string   // substring of some problem when failing
		wantVerdicts []string // restricted per-case verdicts, when checked
	}{
		{
			name: "restricted contains every case", host: baseline, restricted: allContained,
			wantPass: true, wantVerdicts: []string{verdictPass, verdictPass, verdictPass},
		},
		{
			name: "a fully-claimed dimension not contained fails", host: baseline,
			restricted:  restricted(full, fsCase("read", true, true), fsCase("write", true, true), netCase(false, true)),
			wantProblem: "claims network=enforced", wantVerdicts: []string{verdictPass, verdictPass, verdictFail},
		},
		{
			name: "a filesystem escape under a full filesystem claim fails", host: baseline,
			restricted:  restricted(full, fsCase("read", false, true), fsCase("write", true, true), netCase(true, true)),
			wantProblem: "restricted/read: Probe claims filesystem=enforced",
		},
		{
			name: "an unclaimed dimension not contained is honest, not a failure", host: baseline,
			restricted: restricted(noNet, fsCase("read", true, true), fsCase("write", true, true), netCase(false, true)),
			wantPass:   true, wantVerdicts: []string{verdictPass, verdictPass, verdictUnclaimed},
		},
		{
			name: "a partial claim is not held to containment", host: baseline,
			restricted: restricted(partialAll, fsCase("read", false, true), fsCase("write", false, true), netCase(false, true)),
			wantPass:   true, wantVerdicts: []string{verdictUnclaimed, verdictUnclaimed, verdictUnclaimed},
		},
		{
			name: "a legit twin failing on restricted fails", host: baseline,
			restricted:  restricted(full, fsCase("read", true, true), fsCase("write", true, false), netCase(true, true)),
			wantProblem: "restricted/write: legitimate twin failed", wantVerdicts: []string{verdictPass, verdictFail, verdictPass},
		},
		{
			name: "a legit twin failing on the host fails",
			host: host(fsCase("read", false, false), fsCase("write", false, true), netCase(false, true)), restricted: allContained,
			wantProblem: "host/read: legitimate twin failed",
		},
		{
			name: "no restricted backend fails", host: baseline, restricted: nil,
			unavailable: "sandbox: no restricted backend on plan9", wantProblem: "no restricted backend",
		},
		{
			name: "a restricted suite error fails", host: baseline,
			restricted:  &selftestRun{Backend: "restricted", Caps: full, Err: "escape case read: backend could not run it"},
			wantProblem: "restricted backend could not finish",
		},
		{
			name: "a host suite error fails", host: selftestRun{Backend: "host", Err: "boom"}, restricted: allContained,
			wantProblem: "host baseline could not finish",
		},
		{
			name: "containment the host baseline also shows is unproven and fails when claimed",
			host: host(fsCase("read", false, true), fsCase("write", false, true), netCase(true, true)), restricted: allContained,
			wantProblem: "containment unproven", wantVerdicts: []string{verdictPass, verdictPass, verdictUnproven},
		},
		{
			name:       "unproven containment in an unclaimed dimension does not fail",
			host:       host(fsCase("read", false, true), fsCase("write", false, true), netCase(true, true)),
			restricted: restricted(noNet, fsCase("read", true, true), fsCase("write", true, true), netCase(true, true)),
			wantPass:   true, wantVerdicts: []string{verdictPass, verdictPass, verdictUnproven},
		},
		{
			name:       "a skipped network case neither passes nor fails",
			host:       host(fsCase("read", false, true), skippedNet),
			restricted: restricted(full, fsCase("read", true, true), skippedNet),
			wantPass:   true, wantVerdicts: []string{verdictPass, verdictSkipped},
		},
		{
			name: "nothing evaluated fails", host: host(skippedNet), restricted: restricted(full, skippedNet),
			wantProblem: "no escape case was evaluated",
		},
		{
			name: "an unknown kind is held to full enforcement", host: baseline,
			restricted:  restricted(partialAll, mk("fork-bomb", "process", false, true)),
			wantProblem: "claims process=enforced",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := decideSelftest(tt.host, tt.restricted, tt.unavailable)
			if d.Pass != tt.wantPass {
				t.Fatalf("Pass = %v, want %v; problems=%q", d.Pass, tt.wantPass, d.Problems)
			}
			if tt.wantPass && len(d.Problems) > 0 {
				t.Fatalf("a pass must carry no problems: %q", d.Problems)
			}
			if tt.wantProblem != "" && !slices.ContainsFunc(d.Problems, func(p string) bool { return strings.Contains(p, tt.wantProblem) }) {
				t.Fatalf("problems %q lack %q", d.Problems, tt.wantProblem)
			}
			if tt.wantVerdicts != nil {
				var got []string
				for _, v := range d.Restricted {
					got = append(got, v.Verdict)
				}
				if !slices.Equal(got, tt.wantVerdicts) {
					t.Fatalf("restricted verdicts = %q, want %q", got, tt.wantVerdicts)
				}
			}
		})
	}
}

// The rendered verdict never oversells: a pass that relied on unclaimed
// dimensions says the backend is not a full boundary, and a failure lists every
// problem.
func TestSandboxSelftestRenderWording(t *testing.T) {
	caps := sandbox.Capabilities{Backend: "restricted", Isolation: sandbox.IsolationProcess,
		Filesystem: sandbox.EnforcePartial, Network: sandbox.EnforceNone}
	hostRun := selftestRun{Backend: "host", Cases: []escapecore.CaseResult{
		{Name: "read", Kind: escapecore.KindFilesystem, LegitOK: true}}}
	rRun := &selftestRun{Backend: "restricted", Caps: caps, Cases: []escapecore.CaseResult{
		{Name: "read", Kind: escapecore.KindFilesystem, LegitOK: true}}}

	var buf bytes.Buffer
	renderSelftest(&buf, buildSelftestReport(hostRun, rRun, "", decideSelftest(hostRun, rRun, "")))
	if out := buf.String(); !strings.Contains(out, "PASS: ") || !strings.Contains(out, "NOT a full boundary") {
		t.Fatalf("a pass on unclaimed dimensions must say so:\n%s", out)
	}

	buf.Reset()
	renderSelftest(&buf, buildSelftestReport(hostRun, nil, "no restricted backend on plan9",
		decideSelftest(hostRun, nil, "no restricted backend on plan9")))
	if out := buf.String(); !strings.Contains(out, "restricted: UNAVAILABLE") || !strings.Contains(out, "FAIL: 1 problem(s)") {
		t.Fatalf("an OS without a restricted backend must render as a plain failure:\n%s", out)
	}
}

func TestSandboxRunArgs(t *testing.T) {
	type want struct {
		surface, backend, workDir, command string
		timeout                            time.Duration
	}
	tests := []struct {
		name    string
		args    []string
		want    want
		wantErr string
	}{
		{
			name: "surface and a multi-word command",
			args: []string{"--surface", "cron-shell", "--", "echo", "hi"},
			want: want{surface: "cron-shell", command: "echo hi", timeout: 60 * time.Second},
		},
		{
			name: "forced backend, timeout and workdir",
			args: []string{"--surface", "local", "--backend", "restricted", "--timeout", "5s", "--workdir", "/w", "--", "ls", "-la"},
			want: want{surface: "local", backend: "restricted", workDir: "/w", command: "ls -la", timeout: 5 * time.Second},
		},
		{
			name: "one quoted shell line with = flags",
			args: []string{"--surface=discord", "--backend=host", "--", "echo a | wc -c"},
			want: want{surface: "discord", backend: "host", command: "echo a | wc -c", timeout: 60 * time.Second},
		},
		{
			name: "flags after -- belong to the command",
			args: []string{"--surface", "imessage", "--", "grep", "--backend", "x"},
			want: want{surface: "imessage", command: "grep --backend x", timeout: 60 * time.Second},
		},
		{name: "missing --", args: []string{"--surface", "local", "echo", "hi"}, wantErr: "after --"},
		{name: "missing --surface", args: []string{"--", "echo"}, wantErr: "--surface is required"},
		{name: "unknown surface", args: []string{"--surface", "cron_shell", "--", "echo"}, wantErr: `unknown surface "cron_shell"`},
		{name: "unsupported backend", args: []string{"--surface", "local", "--backend", "container", "--", "echo"}, wantErr: "--backend must be host or restricted"},
		{name: "empty command", args: []string{"--surface", "local", "--"}, wantErr: "no command after --"},
		{name: "blank command", args: []string{"--surface", "local", "--", "  "}, wantErr: "no command after --"},
		{name: "stray argument before --", args: []string{"--surface", "local", "extra", "--", "echo"}, wantErr: `unexpected argument "extra"`},
		{name: "non-positive timeout", args: []string{"--surface", "local", "--timeout", "0s", "--", "echo"}, wantErr: "--timeout must be positive"},
		{name: "unknown flag", args: []string{"--surface", "local", "--bogus", "--", "echo"}, wantErr: "bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := parseSandboxRunArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := want{surface: o.surface, backend: o.backend, workDir: o.workDir, command: o.command, timeout: o.timeout}
			if got != tt.want {
				t.Fatalf("parsed %+v, want %+v", got, tt.want)
			}
		})
	}

	o, err := parseSandboxRunArgs([]string{"--config", "/c", "--data", "/d", "--agent", "ops", "--surface", "local", "--", "true"})
	if err != nil || o.dirs.cfgDir != "/c" || o.dirs.dataDir != "/d" || o.dirs.agent != "ops" {
		t.Fatalf("instance flags not bound: err=%v dirs=%+v", err, o.dirs)
	}
}

// run executes through the surface's profile, reports the exit code, cleans up
// its temp work dir, and exits non-zero exactly when the command did.
func TestSandboxRunHost(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	base := []string{"run", "--config", cfg, "--data", data, "--surface", "local", "--backend", "host"}
	ctx := context.Background()

	var out bytes.Buffer
	if err := sandboxMain(ctx, append(base, "--", "echo", "sandbox-run-ok"), &out); err != nil {
		t.Fatalf("clean command must succeed: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "sandbox-run-ok") || !regexp.MustCompile(`(?m)^exit code\s+0$`).MatchString(s) ||
		!regexp.MustCompile(`(?m)^violations\s+none$`).MatchString(s) {
		t.Fatalf("missing output / exit code / violations:\n%s", s)
	}
	if m := regexp.MustCompile(`(?m)^work\s+(\S+) \(fresh temp dir`).FindStringSubmatch(s); m == nil {
		t.Fatalf("a profile without a work dir must run in a fresh temp dir:\n%s", s)
	} else if _, err := os.Stat(m[1]); !os.IsNotExist(err) {
		t.Fatalf("temp work dir %s was not removed (stat err=%v)", m[1], err)
	}

	out.Reset()
	err := sandboxMain(ctx, append(base, "--", "exit 3"), &out)
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("a failing command must fail the run with its exit code, got %v", err)
	}
	if !regexp.MustCompile(`(?m)^exit code\s+3$`).MatchString(out.String()) {
		t.Fatalf("exit code not reported:\n%s", out.String())
	}
}

// A cron-shell trial mirrors the cron spawn site: the environment allowlist
// (a variable the profile's env_pass names passes, anything else is dropped
// whatever its name), and the output cap.
func TestSandboxRunCronShellMirrorsSpawnSite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell $VAR expansion assumes sh")
	}
	t.Setenv("SANDBOX_RUN_PLAIN", "keep-me")
	t.Setenv("SANDBOX_RUN_OTHER", "drop-me")
	t.Setenv("SANDBOX_RUN_API_KEY", "leak-me")
	cfg := t.TempDir()
	stg := &settings{ExecProfiles: map[string]execProfileSetting{surfaceCronShell: {EnvPass: []string{"SANDBOX_RUN_PLAIN"}}}}
	if err := stg.Save(cfg, "main"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { defaultExecProfiles.Load(nil) })
	var out bytes.Buffer
	err := sandboxMain(context.Background(), []string{"run", "--config", cfg, "--data", t.TempDir(),
		"--surface", "cron-shell", "--backend", "host", "--", "echo P=$SANDBOX_RUN_PLAIN O=$SANDBOX_RUN_OTHER K=$SANDBOX_RUN_API_KEY"}, &out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "P=keep-me") || strings.Contains(s, "drop-me") || strings.Contains(s, "leak-me") || !strings.Contains(s, "output cap") {
		t.Fatalf("cron-shell trial must pass env_pass, drop the rest, and cap output:\n%s", s)
	}
}

func TestSandboxDeniedRootOf(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		dir  string
		deny []string
		want string
	}{
		{filepath.Join(data, "cron-work"), []string{"/nonexistent-deny", data}, data}, // not created yet
		{data, []string{data}, data},
		{filepath.Join(root, "database"), []string{data}, ""}, // shared prefix is not containment
		{root, []string{data}, ""},
		{"", []string{data}, ""},
	}
	for _, tt := range tests {
		if got := deniedRootOf(tt.dir, tt.deny); got != tt.want {
			t.Errorf("deniedRootOf(%q, %q) = %q, want %q", tt.dir, tt.deny, got, tt.want)
		}
	}
}

func TestSandboxDispatch(t *testing.T) {
	ctx := context.Background()
	if err := sandboxMain(ctx, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("no subcommand must return usage, got %v", err)
	}
	if err := sandboxMain(ctx, []string{"frobnicate"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("unknown subcommand must be named, got %v", err)
	}
	var help bytes.Buffer
	if err := sandboxMain(ctx, []string{"help"}, &help); err != nil || !strings.Contains(help.String(), "selftest") {
		t.Fatalf("help must print usage and succeed: err=%v out=%q", err, help.String())
	}
}
