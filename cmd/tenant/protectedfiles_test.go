package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/backend/claudecode"
	"tenant/internal/peering"
	githubp "tenant/internal/plugins/github"
	"tenant/internal/plugins/gsuite"
	"tenant/internal/plugins/osys"
	xp "tenant/internal/plugins/x"
)

// controlFilesFixture lays out a config dir holding every protected control file
// and returns the dirs plus each file's original contents.
func controlFilesFixture(t *testing.T) (cfgDir, dataDir string, orig map[string]string) {
	t.Helper()
	base := t.TempDir()
	cfgDir, dataDir = filepath.Join(base, "config"), filepath.Join(base, "data")
	for _, d := range []string{cfgDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stg := &settings{Permissions: map[string]string{catWrite: "allow", catDestructive: "ask"}}
	if err := stg.save(cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{
		launchConfigPath(cfgDir):  "{}",
		credentialsPath(cfgDir):   "{}",
		peering.PeersPath(cfgDir): "[]",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig = map[string]string{}
	for _, p := range []string{settingsPath(cfgDir, "main"), launchConfigPath(cfgDir), credentialsPath(cfgDir), peering.PeersPath(cfgDir)} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		orig[p] = string(b)
	}
	return cfgDir, dataDir, orig
}

// A model turn cannot rewrite Tenant's own control files through the live file
// tools, even with the write category on allow: every path to the settings file
// is refused by os_write / os_append / os_edit / os_mkdir on every surface, as a
// hard boundary the approval broker is never asked about, and the other control
// files likewise. Ordinary writes still go through, and Tenant's own save of the
// settings file is untouched.
func TestFileToolsCannotRewriteControlFiles(t *testing.T) {
	cfgDir, dataDir, orig := controlFilesFixture(t)
	settingsFile := settingsPath(cfgDir, "main")

	broker := newApprovalBroker(nil)
	broker.loadModes(map[string]string{catWrite: "allow"})
	asked := 0
	confirm := func(ctx context.Context, action, detail string) bool {
		asked++
		return broker.Confirm(ctx, action, detail)
	}
	c := &commonFlags{agent: "main", cfgDir: cfgDir, dataDir: dataDir}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{osEnable: true}, confirm,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildToolMux: %v", err)
	}
	defer cleanup()

	dispatch := func(ctx context.Context, name string, args map[string]any) (string, bool) {
		t.Helper()
		b, _ := json.Marshal(args)
		out, isErr, err := mux.Dispatch(ctx, model.ToolCall{Name: name, Arguments: b})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out, isErr
	}
	writes := func(p string) map[string]map[string]any {
		return map[string]map[string]any{
			"os_write_file":  {"path": p, "content": `{"permissions":{"destructive":"allow"}}`},
			"os_append_file": {"path": p, "content": " "},
			"os_edit_file":   {"path": p, "old_string": `"ask"`, "new_string": `"allow"`},
			"os_make_dir":    {"path": p},
		}
	}
	refused := func(ctx context.Context, what, p string) {
		t.Helper()
		for name, args := range writes(p) {
			out, isErr := dispatch(ctx, name, args)
			if !isErr || !strings.Contains(out, "hard boundary") || !strings.Contains(out, "not approvable") {
				t.Errorf("%s via %s must be refused as a hard boundary: isErr=%v %q", name, what, isErr, out)
			}
		}
	}

	ctx := context.Background()
	for _, s := range []string{surfaceLocal, surfaceDiscord, surfaceIMessage, surfaceCronExec} {
		refused(withExecSurface(ctx, s), "absolute path on "+s, settingsFile)
	}
	refused(ctx, "a .. path", filepath.Join(dataDir, "..", "config", "settings.main.json"))
	if err := os.Symlink(cfgDir, filepath.Join(dataDir, "cfg")); err == nil {
		refused(ctx, "a symlinked dir", filepath.Join(dataDir, "cfg", "settings.main.json"))
	} else {
		t.Logf("symlinks unavailable on this host/user, skipping that case: %v", err)
	}
	t.Chdir(cfgDir)
	refused(ctx, "a relative path", "settings.main.json")
	t.Chdir(dataDir)
	refused(ctx, "a relative .. path", filepath.Join("..", "config", "settings.main.json"))
	for p := range orig {
		refused(ctx, filepath.Base(p), p)
	}

	for p, want := range orig {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s changed: %q", filepath.Base(p), b)
		}
	}
	if asked != 0 {
		t.Errorf("the approval broker was asked %d time(s); a hard boundary refuses before asking", asked)
	}

	// Ordinary writes, in the data dir and beside the control files, still go
	// through, approved by the write category.
	for _, p := range []string{filepath.Join(dataDir, "notes.md"), filepath.Join(cfgDir, "scratch.md")} {
		if out, isErr := dispatch(ctx, "os_write_file", map[string]any{"path": p, "content": "hi"}); isErr {
			t.Errorf("an ordinary write to %s must still succeed: %q", p, out)
		}
	}
	if asked != 2 {
		t.Errorf("ordinary writes should each go through the broker once, asked=%d", asked)
	}

	// Tenant's own save does not go through the file tools and still works.
	stg, err := loadSettings(cfgDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	stg.Permissions[catWrite] = "ask"
	if err := stg.save(cfgDir, "main"); err != nil {
		t.Fatalf("Tenant's own settings save must still work: %v", err)
	}
	if got, _ := loadSettings(cfgDir, "main"); got.Permissions[catWrite] != "ask" {
		t.Errorf("own save not persisted: %v", got.Permissions)
	}
}

// The guards do not depend on the write roots: operator-supplied
// --os-write-roots keep them, and `tenant os`, which has no roots at all, has
// them too.
func TestControlFilesProtectedWhateverTheRoots(t *testing.T) {
	cfgDir, dataDir, _ := controlFilesFixture(t)
	settingsFile := settingsPath(cfgDir, "main")
	oneShot := oneShotOSPolicy(cfgDir, dataDir, nil, false, true).Roots
	peerCert, _ := peering.CertPaths(cfgDir)

	for _, tc := range []struct {
		name  string
		roots osys.Roots
		path  string
	}{
		{"default roots", resolveOSRoots(&pluginFlags{}, cfgDir, dataDir, nil), settingsFile},
		{"explicit write roots", resolveOSRoots(&pluginFlags{osWriteRoots: cfgDir}, cfgDir, dataDir, nil), settingsFile},
		{"tenant os (no roots)", oneShot, settingsFile},
		{"another agent's file", oneShot, settingsPath(cfgDir, "helper")},
		{"the settings temp file", oneShot, settingsFile + ".tmp"},
		{"the soul", oneShot, soul.Path(cfgDir, "main")},
		{"the peer cert", oneShot, peerCert},
		{"the spend ledger", oneShot, claudecode.LedgerPath()},
		{"the Tenant binary", oneShot, exePath()},
		{"config.json", oneShot, launchConfigPath(cfgDir)},
	} {
		if err := tc.roots.Check(tc.path, true); err == nil || !strings.Contains(err.Error(), "hard boundary") {
			t.Errorf("%s: %s must be refused as a hard boundary, got %v", tc.name, filepath.Base(tc.path), err)
		}
	}
	if err := oneShot.Check(filepath.Join(dataDir, "out.txt"), true); err != nil {
		t.Errorf("tenant os must still write anywhere else: %v", err)
	}
	if protect, secret := fileToolGuards("", "", nil, nil); slices.ContainsFunc(append(protect, secret...), func(p string) bool {
		return strings.Contains(p, "settings.") || strings.HasSuffix(p, "config.json")
	}) {
		t.Errorf("no config dir, no config-dir guards: %v %v", protect, secret)
	}
}

// fakeHome points the home dir (and the claude CLI's config dir) at a temp dir
// holding a credential store, so the guards built from them are testable.
func fakeHome(t *testing.T) (home, sshKey, claudeCreds string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	claude := filepath.Join(home, "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	sshKey, claudeCreds = filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(claude, ".credentials.json")
	for _, f := range []string{sshKey, claudeCreds} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("s3cret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, sshKey, claudeCreds
}

// A model turn cannot READ Tenant's secrets through the live file tools either:
// config.json (the dashboard token), credentials.json, peers.json, the peer
// key, the OAuth token files, a configured service-account and TLS key, the
// claude CLI's login, and the credential stores the exec sandbox also denies.
// None of it comes back, and ordinary files still read.
func TestFileToolsCannotReadSecrets(t *testing.T) {
	_, sshKey, claudeCreds := fakeHome(t)
	cfgDir, dataDir, _ := controlFilesFixture(t)
	keys := t.TempDir()
	saKey, tlsKey := filepath.Join(keys, "sa[prod].json"), filepath.Join(keys, "dashboard.key")
	_, peerKey := peering.CertPaths(cfgDir)
	secrets := []string{
		launchConfigPath(cfgDir), credentialsPath(cfgDir), peering.PeersPath(cfgDir), peerKey,
		filepath.Join(cfgDir, "mcp", "mcp-0123.json"), atlassianTokenPath(cfgDir),
		githubp.TokenPath(cfgDir), gsuite.EmbeddedOAuthPath(cfgDir),
		// TEN-337: the data-dir X cache and the dashboard token come from
		// the same managedSecrets list doctor audits.
		xp.TokenPath(dataDir), dashboardAuthTokenPath(cfgDir),
		saKey, tlsKey, sshKey, claudeCreds,
	}
	for _, p := range secrets {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("s3cret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lc := &launchConfig{
		Skills:    map[string]*skillConfig{"gsuite": {Settings: map[string]string{"sa_json": saKey}}},
		Dashboard: dashboardConfig{TLSKey: tlsKey},
	}
	c := &commonFlags{agent: "main", cfgDir: cfgDir, dataDir: dataDir, lc: lc}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{osEnable: true}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildToolMux: %v", err)
	}
	defer cleanup()
	read := func(p string) (string, bool) {
		b, _ := json.Marshal(map[string]string{"path": p})
		out, isErr, err := mux.Dispatch(context.Background(), model.ToolCall{Name: "os_read_file", Arguments: b})
		if err != nil {
			t.Fatal(err)
		}
		return out, isErr
	}
	for _, p := range secrets {
		if out, isErr := read(p); !isErr || !strings.Contains(out, "hard boundary") || strings.Contains(out, "s3cret") {
			t.Errorf("reading %s must be refused without leaking: isErr=%v %q", p, isErr, out)
		}
	}
	note := filepath.Join(dataDir, "notes.md")
	if err := os.WriteFile(note, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, isErr := read(note); isErr || out != "hello" {
		t.Errorf("an ordinary file must still read: isErr=%v %q", isErr, out)
	}
}

// The default write roots are Tenant's own dirs and the operator's working
// folders, never a parent of the config or data dir. On a server laid out as
// C:\Tenant\{config,data}, that parent holds the binary and the claude CLI's
// config (whose hooks the claudecode backend would run): neither is writable,
// while the config and data dirs (the wiki lives in data) still are.
func TestDefaultWriteRootsSkipTheParents(t *testing.T) {
	install := t.TempDir() // the C:\Tenant stand-in
	cfgDir, dataDir := filepath.Join(install, "config"), filepath.Join(install, "data")
	for _, d := range []string{cfgDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the temp-dir root and the home folders away from the install.
	tmp := t.TempDir()
	for _, env := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(env, tmp)
	}
	fakeHome(t)

	r := resolveOSRoots(&pluginFlags{}, cfgDir, dataDir, nil)
	if slices.Contains(r.Write, install) {
		t.Errorf("the write roots include the config/data parent %s: %v", install, r.Write)
	}
	for _, want := range []string{cfgDir, dataDir} {
		if !slices.Contains(r.Write, want) {
			t.Errorf("the write roots must still include %s: %v", want, r.Write)
		}
	}
	for _, p := range []string{filepath.Join(install, "tenant.exe"), filepath.Join(install, "claude-config", "settings.json")} {
		if err := r.Check(p, true); err == nil || !strings.Contains(err.Error(), "outside the writable roots") {
			t.Errorf("%s must no longer be writable, got %v", p, err)
		}
	}
	for _, p := range []string{filepath.Join(dataDir, "wiki", "note.md"), filepath.Join(cfgDir, "scratch.md")} {
		if err := r.Check(p, true); err != nil {
			t.Errorf("%s must stay writable: %v", p, err)
		}
	}
}

// On Windows, a PowerShell profile or module under Documents runs in the
// operator's next PowerShell session, so the file tools may not write one even
// though Documents is a write root.
func TestPowerShellProfilesAreProtected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only: PowerShell profiles live under Documents there")
	}
	home, _, _ := fakeHome(t)
	cfgDir, dataDir, _ := controlFilesFixture(t)
	r := resolveOSRoots(&pluginFlags{}, cfgDir, dataDir, nil)
	for _, p := range []string{
		filepath.Join(home, "Documents", "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1"),
		filepath.Join(home, "Documents", "PowerShell", "Modules", "Evil", "Evil.psm1"),
	} {
		if err := r.Check(p, true); err == nil || !strings.Contains(err.Error(), "hard boundary") {
			t.Errorf("%s must be refused as a hard boundary, got %v", p, err)
		}
	}
	if err := r.Check(filepath.Join(home, "Documents", "report.md"), true); err != nil {
		t.Errorf("an ordinary Documents file must stay writable: %v", err)
	}
}
