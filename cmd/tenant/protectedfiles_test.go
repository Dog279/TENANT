package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/peering"
	"tenant/internal/plugins/osys"
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

// The protection does not depend on the write roots: operator-supplied
// --os-write-roots keep it, and `tenant os`, which has no roots at all, has it.
func TestControlFilesProtectedWhateverTheRoots(t *testing.T) {
	cfgDir, dataDir, _ := controlFilesFixture(t)
	settingsFile := settingsPath(cfgDir, "main")
	oneShot := oneShotOSPolicy(cfgDir, false, true).Roots

	for _, tc := range []struct {
		name  string
		roots osys.Roots
		path  string
	}{
		{"default roots", resolveOSRoots(&pluginFlags{}, cfgDir, dataDir), settingsFile},
		{"explicit write roots", resolveOSRoots(&pluginFlags{osWriteRoots: cfgDir}, cfgDir, dataDir), settingsFile},
		{"tenant os (no roots)", oneShot, settingsFile},
		{"another agent's file", oneShot, settingsPath(cfgDir, "helper")},
		{"the settings temp file", oneShot, settingsFile + ".tmp"},
	} {
		if err := tc.roots.Check(tc.path, true); err == nil || !strings.Contains(err.Error(), "hard boundary") {
			t.Errorf("%s: %s must be refused as a hard boundary, got %v", tc.name, filepath.Base(tc.path), err)
		}
	}
	if err := oneShot.Check(filepath.Join(dataDir, "out.txt"), true); err != nil {
		t.Errorf("tenant os must still write anywhere else: %v", err)
	}
	if got := protectedConfigFiles(""); got != nil {
		t.Errorf("no config dir, nothing to protect: %v", got)
	}
}
