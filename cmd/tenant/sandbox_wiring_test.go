package main

import (
	"context"
	"os"
	"path/filepath"
	stdreflect "reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/plugins/osys"
	"tenant/internal/sandbox"
	"tenant/internal/tui"
)

// resetExecProfiles restores the process-wide store after a test that loads it.
func resetExecProfiles(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { defaultExecProfiles.Load(nil) })
}

// restrictedOrSkip skips unless this OS's restricted backend enforces
// filesystem roots: the tests that use it need a real refusal, and Windows
// enforces none until TEN-346 lands its ACEs.
func restrictedOrSkip(t *testing.T) {
	t.Helper()
	b, err := sandbox.Restricted()
	if err != nil {
		t.Skipf("no restricted backend on this OS: %v", err)
	}
	c := b.Probe(context.Background())
	if c.Filesystem == sandbox.EnforceNone {
		t.Skipf("the restricted backend enforces no filesystem roots here: %s", c.Detail)
	}
	// These tests author POSIX-sh commands with the host's native paths, so they
	// only apply when the backend runs the host's native shell. On Windows the
	// restricted backend runs sh inside WSL with Linux paths (Capabilities.Shell
	// == "sh"), so its refusals are covered by the sandbox package's own live WSL
	// tests instead.
	native := "sh"
	if runtime.GOOS == "windows" {
		native = "powershell"
	}
	if c.Shell != "" && c.Shell != native {
		t.Skipf("restricted backend runs %q, not this host's %q shell; its containment is tested in internal/sandbox", c.Shell, native)
	}
}

func TestExecSurface_ContextStamp(t *testing.T) {
	ctx := context.Background()
	if got := execSurfaceFrom(ctx); got != surfaceLocal {
		t.Fatalf("unstamped ctx = %q, want local", got)
	}
	d := withExecSurface(ctx, surfaceDiscord)
	child, cancel := context.WithCancel(d) // tool calls and sub-contexts inherit it
	defer cancel()
	if got := execSurfaceFrom(child); got != surfaceDiscord {
		t.Fatalf("derived ctx lost the surface: %q", got)
	}
}

func TestInheritExecSurface_SubAgentKeepsParentSurface(t *testing.T) {
	base := context.Background() // a team runtime's long-lived BaseCtx
	turn := withExecSurface(context.Background(), surfaceDiscord)
	if got := execSurfaceFrom(inheritExecSurface(base, turn)); got != surfaceDiscord {
		t.Fatalf("sub-agent base ctx must keep the parent's surface, got %q", got)
	}
	if got := execSurfaceFrom(inheritExecSurface(base, context.Background())); got != surfaceLocal {
		t.Fatalf("an unstamped parent leaves the sub-agent local, got %q", got)
	}
}

func TestExecProfileStore_LoadValidates(t *testing.T) {
	resetExecProfiles(t)
	n, skipped := defaultExecProfiles.Load(map[string]execProfileSetting{
		"cron-shell": {Backend: "restricted", Network: "none"},
		"default":    {Backend: "host"},
		"bogus":      {Backend: "restricted"},
		"discord":    {Backend: "container"},
		"imessage":   {Network: "sideways"},
		"local":      {WriteRoots: []string{"relative/dir"}},
		"cron-exec":  {MaxPIDs: -1},
	})
	if n != 2 || len(skipped) != 5 {
		t.Fatalf("applied %d, skipped %v; want 2 applied, 5 skipped", n, skipped)
	}
	for _, want := range []string{"unknown surface", "container backend is not implemented", "unknown network", "must be absolute", "must not be negative"} {
		if !strings.Contains(strings.Join(skipped, "\n"), want) {
			t.Errorf("skip messages should mention %q: %v", want, skipped)
		}
	}
}

func TestResolveExecProfile_AppliesOverrides(t *testing.T) {
	resetExecProfiles(t)
	off := false
	secrets := filepath.Join(t.TempDir(), "secrets") // absolute on every OS; /opt/secrets is not on Windows
	defaultExecProfiles.Load(map[string]execProfileSetting{
		"cron-shell": {
			Backend: "restricted", Enforce: &off, Network: "none",
			WriteRoots: []string{"~/tenant-out"}, DenyRoots: []string{secrets},
			EnvPass: []string{"PATH"}, EnvSet: map[string]string{"LANG": "C"},
			CPUSeconds: 30, MemoryMB: 512, MaxPIDs: 64,
		},
	})
	cfg, data := t.TempDir(), t.TempDir()
	p := resolveExecProfile(surfaceCronShell, cfg, data, "/work")
	home, _ := os.UserHomeDir()

	if p.Backend != "restricted" || p.Policy.Enforce || p.Policy.Network != sandbox.NetNone || p.Policy.WorkDir != "/work" {
		t.Fatalf("override not applied: %+v", p)
	}
	if !stdreflect.DeepEqual(p.Policy.WriteRoots, []string{filepath.Join(home, "tenant-out")}) {
		t.Errorf("~ not expanded in write roots: %v", p.Policy.WriteRoots)
	}
	for _, must := range []string{cfg, data, secrets, filepath.Join(home, ".ssh"), filepath.Join(home, ".aws")} {
		if !hasString(p.Policy.DenyRoots, must) {
			t.Errorf("deny roots missing %q: %v", must, p.Policy.DenyRoots)
		}
	}
	if !p.Policy.Env.Isolate || p.Policy.Env.Set["LANG"] != "C" || !hasString(p.Policy.Env.Pass, "PATH") {
		t.Errorf("env allowlist not applied: %+v", p.Policy.Env)
	}
	if p.Policy.Limits.CPU != 30*time.Second || p.Policy.Limits.MemoryBytes != 512<<20 || p.Policy.Limits.MaxPIDs != 64 {
		t.Errorf("limits not applied: %+v", p.Policy.Limits)
	}
	// Host profiles do not grow the credential-store list (host enforces nothing).
	if hp := resolveExecProfile(surfaceLocal, cfg, data, ""); hasString(hp.Policy.DenyRoots, filepath.Join(home, ".ssh")) {
		t.Errorf("host profile should keep the built-in deny set only: %v", hp.Policy.DenyRoots)
	}
}

// Every profile, host included, gives commands the safe-env allowlist: PATH
// kept, anything unlisted dropped whether or not its name looks secret.
func TestResolveExecProfile_EnvAllowlistOnEveryBackend(t *testing.T) {
	resetExecProfiles(t)
	t.Setenv("TENANT_TEST_API_KEY", "leak-me")
	t.Setenv("TENANT_TEST_DB_URL", "postgres://u:pw@h/db") // a secret with an innocent name
	defaultExecProfiles.Load(map[string]execProfileSetting{"discord": {Backend: "restricted"}, "local": {Backend: "host"}})
	for _, surface := range []string{surfaceDiscord, surfaceLocal, surfaceCronShell} {
		p := resolveExecProfile(surface, "", "", "")
		if !p.Policy.Env.Isolate || hasString(p.Policy.Env.Pass, "TENANT_TEST_API_KEY") ||
			hasString(p.Policy.Env.Pass, "TENANT_TEST_DB_URL") || !slices.ContainsFunc(p.Policy.Env.Pass, func(n string) bool { return strings.EqualFold(n, "PATH") }) {
			t.Fatalf("%s (%s): the env must be the allowlist: %+v", surface, p.Backend, p.Policy.Env)
		}
	}
}

func TestWithBackend_ForcedRestrictedGetsConfinedDefaults(t *testing.T) {
	resetExecProfiles(t)
	home, _ := os.UserHomeDir()
	p := withBackend(resolveExecProfile(surfaceCronShell, "", "", ""), "restricted")
	if p.Backend != "restricted" || !hasString(p.Policy.DenyRoots, filepath.Join(home, ".ssh")) || !p.Policy.Env.Isolate {
		t.Fatalf("a forced restricted run must carry the confined defaults (credential deny + scrubbed env): %+v", p)
	}
	if h := withBackend(p, "host"); h.Backend != "host" {
		t.Fatalf("withBackend(host) = %q", h.Backend)
	}
}

func TestResolveExecProfile_SurfaceEntryBeatsDefault(t *testing.T) {
	resetExecProfiles(t)
	defaultExecProfiles.Load(map[string]execProfileSetting{
		"default": {Backend: "restricted"},
		"local":   {Backend: "host"},
	})
	if b := resolveExecProfile(surfaceLocal, "", "", "").Backend; b != "host" {
		t.Errorf("local's own entry must win, got %q", b)
	}
	if b := resolveExecProfile(surfaceDiscord, "", "", "").Backend; b != "restricted" {
		t.Errorf("surfaces without an entry take default, got %q", b)
	}
	defaultExecProfiles.Load(nil)
	if p := resolveExecProfile(surfaceDiscord, "", "", ""); p.Backend != "host" || p.Policy.Enforce {
		t.Errorf("no overrides ⇒ the built-in backend (pinned host in tests) in dry run, got %+v", p)
	}
}

// The os_exec path through the seam must be indistinguishable from osys's own
// realRunner on the host profile: same output, same error text, same timeout
// message. Compared through the public osys API, side by side.
func TestProfileRunner_HostMatchesRealRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based comparison; the Windows shell path is covered by osys tests")
	}
	cfg := osys.Config{ExecTimeout: 200 * time.Millisecond}
	legacy, err := osys.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runner = profileRunner(resolveExecProfile(surfaceLocal, "", "", ""), surfaceLocal, nil)
	seam, err := osys.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"echo hello", "echo out; exit 3", "echo boom >&2; exit 1", "sleep 5"} {
		lo, le := legacy.Exec(context.Background(), cmd)
		so, se := seam.Exec(context.Background(), cmd)
		if lo != so || errText(le) != errText(se) {
			t.Errorf("%q: legacy=(%q, %q) seam=(%q, %q)", cmd, lo, errText(le), so, errText(se))
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// A cron job keeps cmd /C only on the Windows host (a surface pinned there);
// in the WSL sandbox it runs as sh, as everywhere else.
func TestCronShellArgv_CmdOnlyOnTheWindowsHost(t *testing.T) {
	for _, tc := range []struct {
		goos, backend string
		want          []string
	}{
		{"windows", "host", []string{"cmd", "/C", "dir"}},
		{"windows", "restricted", []string{"sh", "-c", "dir"}},
		{"linux", "host", []string{"sh", "-c", "dir"}},
		{"darwin", "restricted", []string{"sh", "-c", "dir"}},
	} {
		if got := cronShellArgvFor(tc.goos, tc.backend, "dir"); !stdreflect.DeepEqual(got, tc.want) {
			t.Errorf("%s/%s: %v, want %v", tc.goos, tc.backend, got, tc.want)
		}
	}
}

func TestRunSandboxShell_ExitTextParity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based")
	}
	_, err := runSandboxShell(context.Background(), "exit 3", resolveExecProfile(surfaceCronShell, "", t.TempDir(), t.TempDir()))
	if err == nil || err.Error() != "command failed: exit status 3" {
		t.Fatalf("want the pre-seam text \"command failed: exit status 3\", got %v", err)
	}
}

func TestCategorize_ExecUnsandboxedHasItsOwnCategory(t *testing.T) {
	if got := categorize(actionExecUnsandboxed); got != catSandbox {
		t.Fatalf("categorize(%q) = %q, want %q", actionExecUnsandboxed, got, catSandbox)
	}
}

// Approving one sandbox escalation "for the session" must grant only further
// escalations. Filed under destructive, it granted every irreversible action.
func TestBroker_SessionGrantOnEscalationDoesNotGrantDestructive(t *testing.T) {
	b := newApprovalBroker(nil)
	b.SetRules(nil) // no per-tool rules: exercise the category logic alone
	b.SetAsk(func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision { return tui.ApproveSession })
	if !b.Confirm(context.Background(), actionExecUnsandboxed, "cat x  [SANDBOX REFUSED: ...]") {
		t.Fatal("the escalation itself was approved for the session")
	}
	asked := false
	b.SetAsk(func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision { asked = true; return tui.DenyOnce })
	if b.Confirm(context.Background(), "os_exec_dangerous", "rm -rf /  [DANGER]") || !asked {
		t.Fatalf("a session grant on a sandbox escalation must not pre-approve destructive actions (asked=%v)", asked)
	}
	asked = false
	if !b.Confirm(context.Background(), actionExecUnsandboxed, "cat y") || asked {
		t.Fatalf("further escalations should ride the session grant (asked=%v)", asked)
	}
}

func TestCronExecApprover_DeniesSandboxEscalation(t *testing.T) {
	if cronExecApprover(t.TempDir(), t.TempDir())(context.Background(), actionExecUnsandboxed, "cat x") {
		t.Fatal("an unattended job must never re-run a refused command outside the sandbox")
	}
}

func TestExecProfileStore_RejectsUnenforcedAllowlist(t *testing.T) {
	resetExecProfiles(t)
	n, skipped := defaultExecProfiles.Load(map[string]execProfileSetting{
		"discord":  {Backend: "restricted", Network: "allowlist"},
		"imessage": {Backend: "restricted", Allow: []string{"example.com"}},
	})
	if n != 0 || len(skipped) != 2 || !strings.Contains(strings.Join(skipped, " "), "not enforced by any backend") {
		t.Fatalf("an allowlist must be rejected until something enforces it: n=%d skipped=%v", n, skipped)
	}
}

func TestApplySettingsFile_ReloadsExecProfiles(t *testing.T) {
	resetExecProfiles(t)
	cfg := t.TempDir()
	rules := newToolRules()
	broker := newApprovalBroker(nil)
	broker.SetRules(rules)
	var mu sync.Mutex
	stg := &settings{Tools: map[string]bool{}}

	t.Cleanup(func() { defaultExecProfiles.LoadExceptions(nil) })
	body := `{"tools":{},"exec_profiles":{"cron-shell":{"backend":"restricted","network":"none"},"bogus":{"backend":"host"}},` +
		`"exec_exceptions":{"discord":["~/tenant-out"]}}`
	if err := os.WriteFile(filepath.Join(cfg, "settings.main.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := applySettingsFile(cfg, "main", broker, rules, &mu, stg)
	if !strings.Contains(got, "1 exec profile(s) (1 invalid skipped") || !strings.Contains(got, "1 sandbox exception(s)") {
		t.Fatalf("summary = %q", got)
	}
	if p, ok := defaultExecProfiles.lookup(surfaceCronShell); !ok || p.Backend != "restricted" {
		t.Fatalf("the edited profile must be live: %+v ok=%v", p, ok)
	}
	if home, _ := os.UserHomeDir(); !hasString(defaultExecProfiles.exceptions(surfaceDiscord), filepath.Join(home, "tenant-out")) {
		t.Fatalf("the edited exception must be live, ~ expanded: %v", defaultExecProfiles.exceptions(surfaceDiscord))
	}
	if len(stg.ExecProfiles) != 2 || len(stg.ExecExceptions) != 1 {
		t.Errorf("stg must track the FILE (invalid entry kept, so a save never deletes it): %v %v", stg.ExecProfiles, stg.ExecExceptions)
	}
	if again := applySettingsFile(cfg, "main", broker, rules, &mu, stg); again != "" {
		t.Errorf("unchanged file must be a no-op, got %q", again)
	}
}

// The unattended exec approver must refuse writes that RESOLVE into Tenant's own
// config/data dirs, not just the literal absolute path. These details are built
// so the old substring check (mentionsDir) provably misses them.
func TestCronExecApprover_ResolvedSelfModificationGuard(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	ap := cronExecApprover(cfg, data)
	ctx := context.Background()

	link := filepath.Join(t.TempDir(), "innocent")
	if err := os.Symlink(cfg, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dotdot := filepath.Dir(cfg) + string(filepath.Separator) + "elsewhere" + string(filepath.Separator) + ".." +
		string(filepath.Separator) + filepath.Base(cfg) + string(filepath.Separator) + "config.json"

	for _, detail := range []string{
		"OVERWRITE existing file " + filepath.Join(link, "config.json"),
		"write " + dotdot,
	} {
		if mentionsDir(detail, cfg) || mentionsDir(detail, data) {
			t.Fatalf("test construction: %q must evade the substring check", detail)
		}
		if ap(ctx, "os_write", detail) {
			t.Errorf("approver allowed a write that resolves into the config dir: %q", detail)
		}
	}
	if !ap(ctx, "os_write", "write "+filepath.Join(t.TempDir(), "report.txt")) {
		t.Error("a benign write must still be allowed")
	}
}

// --- live, macOS (skipped where no restricted backend exists) ---

func secretFixture(t *testing.T) (dir, file string) {
	dir = t.TempDir()
	file = filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(file, []byte("S3CRET-VALUE"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func TestProfileRunner_RefusalEscalatesThroughConfirm(t *testing.T) {
	restrictedOrSkip(t)
	dir, file := secretFixture(t)
	p := execProfile{Backend: "restricted", Policy: sandbox.Policy{WorkDir: t.TempDir(), DenyRoots: []string{dir}, Enforce: true}}

	var action, detail string
	deny := func(_ context.Context, a, d string) bool { action, detail = a, d; return false }
	out, err := profileRunner(p, surfaceLocal, deny)(context.Background(), "cat "+file, 10*time.Second)
	if action != actionExecUnsandboxed || !strings.Contains(detail, "SANDBOX REFUSED") {
		t.Fatalf("refusal must escalate as %s with the refusal named; got action=%q detail=%q", actionExecUnsandboxed, action, detail)
	}
	if err == nil || !strings.Contains(err.Error(), "sandbox refused") || strings.Contains(out, "S3CRET") {
		t.Fatalf("denied escalation must stay confined and name the refusal: out=%q err=%v", out, err)
	}

	allow := func(context.Context, string, string) bool { return true }
	out, err = profileRunner(p, surfaceLocal, allow)(context.Background(), "cat "+file, 10*time.Second)
	if err != nil || !strings.Contains(out, "S3CRET-VALUE") || !strings.Contains(out, "OUTSIDE the sandbox after approval") {
		t.Fatalf("approved escalation must re-run on the host and say so: out=%q err=%v", out, err)
	}
}

func TestProfileRunner_DryRunRunsOnHostWithoutAsking(t *testing.T) {
	restrictedOrSkip(t)
	dir, file := secretFixture(t)
	p := execProfile{Backend: "restricted", Policy: sandbox.Policy{WorkDir: t.TempDir(), DenyRoots: []string{dir}, Enforce: false}}
	never := func(context.Context, string, string) bool { t.Fatal("dry-run must not ask"); return false }
	out, err := profileRunner(p, surfaceLocal, never)(context.Background(), "cat "+file, 10*time.Second)
	if err != nil || !strings.Contains(out, "S3CRET-VALUE") || !strings.Contains(out, "sandbox dry-run") {
		t.Fatalf("dry-run must run on the host and note the would-be refusal: out=%q err=%v", out, err)
	}
}

func TestRunSandboxShell_RestrictedRefusalFailsTheJob(t *testing.T) {
	restrictedOrSkip(t)
	dir, file := secretFixture(t)
	p := execProfile{Backend: "restricted", Policy: sandbox.Policy{WorkDir: t.TempDir(), DenyRoots: []string{dir}, Enforce: true}}
	out, err := runSandboxShell(context.Background(), "cat "+file, p)
	if err == nil || !strings.Contains(err.Error(), "sandbox refused") || strings.Contains(out, "S3CRET") {
		t.Fatalf("an unattended refusal must fail the job with the refusal named: out=%q err=%v", out, err)
	}
}

// One shared os service, two surfaces: the profile is resolved per call from the
// turn's context.
func TestSandboxRunner_ResolvesProfilePerSurface(t *testing.T) {
	restrictedOrSkip(t)
	resetExecProfiles(t)
	defaultExecProfiles.Load(map[string]execProfileSetting{"discord": {Backend: "restricted"}})
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte("CFG-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := sandboxRunner(cfg, t.TempDir(), nil)
	cmd := "cat " + filepath.Join(cfg, "config.json")

	if out, err := run(context.Background(), cmd, 10*time.Second); err != nil || !strings.Contains(out, "CFG-CONTENT") {
		t.Fatalf("local surface is host (unconfined): out=%q err=%v", out, err)
	}
	out, err := run(withExecSurface(context.Background(), surfaceDiscord), cmd, 10*time.Second)
	if err == nil || strings.Contains(out, "CFG-CONTENT") {
		t.Fatalf("discord surface is restricted and cfgDir is a deny root: out=%q err=%v", out, err)
	}
}

func hasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
