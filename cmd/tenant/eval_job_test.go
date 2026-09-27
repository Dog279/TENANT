package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/eval"
	"tenant/internal/model"
)

// TestEvalNightlyJob_SmokeArtifact runs the nightly job against the smoke
// subset (fixture mode — no model/tools/judge), so it deterministically
// exercises the Job mechanics: run -> write artifact -> JobResult. Live
// subsets need a model and are covered by the cmdEval path.
func TestEvalNightlyJob_SmokeArtifact(t *testing.T) {
	tmp := t.TempDir()
	j := &evalNightlyJob{
		c:            &commonFlags{dataDir: tmp},
		pf:           &pluginFlags{},
		subset:       eval.SubsetSmoke,
		artifactDir:  filepath.Join(tmp, "artifacts"),
		baselinePath: filepath.Join(tmp, "does-not-exist.json"), // no check
		log:          slog.Default(),
	}

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed {
		t.Error("eval job must report Changed=false (it observes, never mutates)")
	}
	if got, _ := res.Details["overall"].(float64); got != 100 {
		t.Errorf("overall = %v, want 100 (smoke fixtures all pass)", res.Details["overall"])
	}
	if got, _ := res.Details["regressed"].(bool); got {
		t.Error("regressed = true, want false (no baseline present)")
	}

	// Exactly one timestamped report artifact must have been written (the
	// nightly also appends trend.jsonl in the same dir — TEN-158 — which is not
	// a report artifact and is excluded here).
	entries, err := os.ReadDir(j.artifactDir)
	if err != nil {
		t.Fatalf("read artifact dir: %v", err)
	}
	reports, sawTrend := 0, false
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "eval-") && strings.HasSuffix(e.Name(), ".json"):
			reports++
		case e.Name() == "trend.jsonl":
			sawTrend = true
		}
	}
	if reports != 1 {
		t.Fatalf("want exactly 1 report artifact in %s, got %d (entries: %v)", j.artifactDir, reports, entries)
	}
	if !sawTrend {
		t.Error("nightly run should have appended trend.jsonl (TEN-158)")
	}
}

// The eval harness must hand task agents a READ/COMMS-SAFE surface no matter
// which allow flags the hosting process (serve) was launched with. buildToolMux
// honors operator flags, so before this pin the nightly eval inherited
// --os-allow-write and a failing restraint task (fitness-051's pass criterion
// is proposing an edit WITHOUT writing) actually patched
// internal/model/profile.go in the production source tree.
func TestEvalSurfaceIsReadCommsSafe(t *testing.T) {
	mux := newToolMux()
	mux.Add("os", stubPlugin{Specs: []model.ToolSpec{
		{Name: "os_read_file"},
		{Name: "os_write_file", Gated: true},
		{Name: "os_exec", Gated: true},
	}})
	mux.Add("discord", stubPlugin{Specs: []model.ToolSpec{
		{Name: "discord_send_message", Gated: true},
	}})
	mux.Add("team", stubPlugin{Specs: []model.ToolSpec{
		{Name: "spawn_agent"},
		{Name: "team_await"},
	}})

	view, disp := restrictReadComms(mux, mux, "quality evals", nil)
	surface := &evalSurface{view, disp}

	got := map[string]bool{}
	for _, sp := range surface.All() {
		got[sp.Name] = true
	}
	if !got["os_read_file"] {
		t.Error("read tool missing from the eval surface")
	}
	for _, banned := range []string{"os_write_file", "os_exec", "discord_send_message", "spawn_agent", "team_await"} {
		if got[banned] {
			t.Errorf("%s is exposed to eval agents — an eval run can act on the world", banned)
		}
	}

	// The dispatcher refuses by name too (defense against a model calling an
	// unlisted tool it hallucinated or remembered from another surface).
	out, handled, err := surface.Dispatch(context.Background(), model.ToolCall{Name: "os_write_file"})
	if err != nil || !handled {
		t.Fatalf("refusal should be a handled tool result, got handled=%v err=%v", handled, err)
	}
	if !strings.Contains(out, "read/research-only") {
		t.Errorf("refusal should explain the surface, got: %s", out)
	}
}
