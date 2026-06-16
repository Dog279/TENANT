package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/model"
)

// TEN-91: attachJudge generalizes beyond Anthropic — it builds a RoleJudge
// profile for any wired provider kind and registers the right backend.
func TestAttachJudge_PerKind(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		kind, model, wantBackend string
	}{
		{"anthropic", "claude-opus-4-8", "anthropic"},
		{"zai", "glm-4.6", "vllm"},     // OpenAI-compatible (keyed)
		{"openai", "gpt-4o", "vllm"},   // OpenAI-compatible (keyed)
		{"ollama", "llama3.1", "vllm"}, // KEYLESS local judge (TEN-91 review fix)
	}
	for _, tc := range cases {
		r := model.NewRouter(model.NewEmptyRegistry(), nil)
		if err := attachJudge(r, tc.kind, "", tc.model, "test-key"); err != nil {
			t.Fatalf("attachJudge(%s): %v", tc.kind, err)
		}
		_, prof, err := r.LLMForRole(ctx, model.RoleJudge)
		if err != nil {
			t.Fatalf("LLMForRole(judge) after attachJudge(%s): %v", tc.kind, err)
		}
		if prof.Model != tc.model {
			t.Errorf("%s: judge model = %q, want %q", tc.kind, prof.Model, tc.model)
		}
		if prof.Backend != tc.wantBackend {
			t.Errorf("%s: judge backend = %q, want %q", tc.kind, prof.Backend, tc.wantBackend)
		}
		if prof.Role != model.RoleJudge {
			t.Errorf("%s: role = %q, want judge", tc.kind, prof.Role)
		}
	}
}

func TestAttachJudge_UnknownKind(t *testing.T) {
	r := model.NewRouter(model.NewEmptyRegistry(), nil)
	if err := attachJudge(r, "nope", "", "m", "k"); err == nil || !strings.Contains(err.Error(), "unknown judge provider kind") {
		t.Fatalf("unknown kind should error, got %v", err)
	}
}

// TEN-91 review fix: resolveJudgeKey requires a key only for KEYED kinds; a
// keyless local judge (ollama/vllm) needs none and must not error.
func TestResolveJudgeKey(t *testing.T) {
	// Keyless local kind → no key, no error (the review-found bug: this used to
	// fail with a bogus ANTHROPIC_API_KEY error).
	if key, err := resolveJudgeKey("ollama", "ANTHROPIC_API_KEY"); err != nil || key != "" {
		t.Fatalf("keyless ollama judge should need no key: key=%q err=%v", key, err)
	}
	// Keyed kind with the env unset → clear error naming the kind's env var.
	t.Setenv("ZAI_API_KEY", "")
	if _, err := resolveJudgeKey("zai", "ANTHROPIC_API_KEY"); err == nil || !strings.Contains(err.Error(), "ZAI_API_KEY") {
		t.Fatalf("zai judge with no key should error naming ZAI_API_KEY, got %v", err)
	}
	// Keyed kind with the env set → returns the key.
	t.Setenv("ZAI_API_KEY", "sk-zzz")
	if key, err := resolveJudgeKey("zai", "ANTHROPIC_API_KEY"); err != nil || key != "sk-zzz" {
		t.Fatalf("zai judge should read $ZAI_API_KEY: key=%q err=%v", key, err)
	}
	// Explicit --judge-key-env override wins over the catalog default.
	t.Setenv("MY_JUDGE_KEY", "sk-custom")
	if key, err := resolveJudgeKey("anthropic", "MY_JUDGE_KEY"); err != nil || key != "sk-custom" {
		t.Fatalf("explicit key-env should win: key=%q err=%v", key, err)
	}
	// Unknown kind → error.
	if _, err := resolveJudgeKey("bogus", ""); err == nil {
		t.Fatal("unknown kind should error")
	}
}

// applyJudgeConfig: flag wins; empty config is a no-op; config fills when no flag.
func TestApplyJudgeConfig(t *testing.T) {
	lc := &launchConfig{}
	lc.Improve.Judge = "claude-opus-4-8"
	lc.Improve.JudgeKind = "anthropic"
	lc.Improve.JudgeEndpoint = "https://example/api"
	lc.Improve.JudgeKeyEnv = "MY_KEY"

	// No flag → filled from config.
	got := applyJudgeConfig(evalJudgeOpts{}, lc)
	if got.model != "claude-opus-4-8" || got.kind != "anthropic" || got.endpoint != "https://example/api" || got.keyEnv != "MY_KEY" {
		t.Fatalf("config not applied: %+v", got)
	}

	// Flag set → untouched (flag wins).
	got = applyJudgeConfig(evalJudgeOpts{model: "flag-model", keyEnv: "FLAG_KEY"}, lc)
	if got.model != "flag-model" || got.keyEnv != "FLAG_KEY" {
		t.Fatalf("flag must win over config: %+v", got)
	}

	// Empty config → unchanged (planner default).
	if got := applyJudgeConfig(evalJudgeOpts{}, &launchConfig{}); got.model != "" {
		t.Fatalf("empty config must leave planner default: %+v", got)
	}
	// nil lc → unchanged.
	if got := applyJudgeConfig(evalJudgeOpts{}, nil); got.model != "" {
		t.Fatalf("nil lc must be a no-op: %+v", got)
	}
}

// judgeCtl persists set/clear round-trip + Current reflects state.
func TestJudgeCtl_Persist(t *testing.T) {
	dir := t.TempDir()
	j := judgeCtl{cfgDir: dir, planner: "qwen-local"}

	if c := j.Current(); !strings.Contains(c, "qwen-local") || !strings.Contains(strings.ToLower(c), "default") {
		t.Errorf("default Current should mention the planner + default: %q", c)
	}

	if _, err := j.Set("anthropic", "claude-opus-4-8", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lc.Improve.Judge != "claude-opus-4-8" || lc.Improve.JudgeKind != "anthropic" || lc.Improve.JudgeKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("Set didn't persist correctly: %+v", lc.Improve)
	}
	if c := j.Current(); !strings.Contains(c, "claude-opus-4-8") {
		t.Errorf("Current should show the set judge: %q", c)
	}

	// Unknown kind → error, no persist change.
	if _, err := j.Set("bogus", "x", ""); err == nil {
		t.Error("Set with unknown kind should error")
	}

	if err := j.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	lc, _ = loadLaunchConfig(dir)
	if lc.Improve.Judge != "" || lc.Improve.JudgeKind != "" {
		t.Fatalf("Clear didn't reset: %+v", lc.Improve)
	}
}
