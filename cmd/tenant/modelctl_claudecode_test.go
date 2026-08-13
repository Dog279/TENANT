package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// Keyless catalog add (claudecode): `/model add claudecode` and
// `/model add-cloud claudecode <stray-key>` must both register the provider
// from the catalog with no credential stored, while keyed kinds keep
// demanding a key and non-catalog names keep demanding an endpoint.
func TestAddClaudecodeKeyless(t *testing.T) {
	dir := t.TempDir()
	(&launchConfig{}).save(dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := newEchoAgent(t, log)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag, log: log}

	// TUI path: /model add claudecode (empty endpoint → keyless catalog add).
	status, err := mc.AddModel("claudecode", "", "")
	if err != nil {
		t.Fatalf("AddModel keyless: %v", err)
	}
	if !strings.Contains(status, "claudecode") {
		t.Fatalf("status doesn't mention the provider: %s", status)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	pc := lc.Providers["claudecode"]
	if pc == nil || pc.Kind != "claudecode" || pc.Model != "opus" {
		t.Fatalf("provider not registered from catalog: %+v", pc)
	}
	if pc.Auth.Stored {
		t.Fatalf("keyless provider must not claim a stored credential: %+v", pc)
	}

	// add-cloud with a mistakenly-supplied key: succeeds, key NOT stored.
	status2, err := mc.AddCloudModel("claudecode", "sk-oops")
	if err != nil {
		t.Fatalf("AddCloudModel keyless: %v", err)
	}
	if !strings.Contains(status2, "not stored") {
		t.Fatalf("expected the not-stored note, got: %s", status2)
	}
	lc2, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lc2.Providers["claudecode"].Auth.Stored {
		t.Fatal("stray key must not be stored")
	}

	// Keyed kinds unchanged: anthropic without a key still errors.
	if _, err := mc.AddCloudModel("anthropic", ""); err == nil {
		t.Fatal("anthropic without key must error")
	}

	// Non-catalog names still require an endpoint.
	if _, err := mc.AddModel("mybox", "", ""); err == nil {
		t.Fatal("empty endpoint for a non-catalog name must error")
	}
}

// The claudecode kind exposes the Claude CLI's full effort ladder (max is a
// real level there), while Fugu keeps its high/xhigh pair with max aliased.
func TestReasoningLevelsPerKind(t *testing.T) {
	got := reasoningLevelsForKind("claudecode")
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if len(got) != len(want) {
		t.Fatalf("claudecode levels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("claudecode levels = %v, want %v", got, want)
		}
	}
	if f := reasoningLevelsForKind("sakana"); len(f) != 2 || f[0] != "high" || f[1] != "xhigh" {
		t.Fatalf("fugu levels changed: %v", f)
	}
}

// SetReasoningEffort on an active claudecode provider: max passes through
// un-aliased, low is legal, and the value persists per-provider.
func TestSetReasoningEffortClaudecode(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "claudecode",
		Providers: map[string]*providerConfig{
			"claudecode": {Kind: "claudecode", Endpoint: "claudecode://auto", Model: "opus"},
		},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := newEchoAgent(t, log)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag, log: log}

	status, err := mc.SetReasoningEffort("max")
	if err != nil {
		t.Fatalf("SetReasoningEffort(max): %v", err)
	}
	if !strings.Contains(status, "max") {
		t.Fatalf("max was aliased away for claudecode: %s", status)
	}
	lc2, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lc2.Providers["claudecode"].Reasoning != "max" {
		t.Fatalf("persisted reasoning = %q, want max", lc2.Providers["claudecode"].Reasoning)
	}
	if _, err := mc.SetReasoningEffort("low"); err != nil {
		t.Fatalf("low must be legal for claudecode: %v", err)
	}
	if _, err := mc.SetReasoningEffort("ultra"); err == nil {
		t.Fatal("bogus level must error")
	}
}

// ListProviderModels for claudecode returns the static CLI aliases instead
// of attempting an HTTP catalog fetch against the discovery sentinel.
func TestListProviderModelsClaudecode(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "claudecode",
		Providers: map[string]*providerConfig{
			"claudecode": {Kind: "claudecode", Endpoint: "claudecode://auto", Model: "opus"},
		},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := newEchoAgent(t, log)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag, log: log}

	ids, err := mc.ListProviderModels("claudecode")
	if err != nil {
		t.Fatalf("ListProviderModels: %v", err)
	}
	if len(ids) != 4 || ids[0] != "fable" || ids[3] != "sonnet" {
		t.Fatalf("unexpected alias list: %v", ids)
	}
}
