package route_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/route"
)

type classifierFunc func(context.Context, string) (route.Signal, error)

func (f classifierFunc) Classify(ctx context.Context, prompt string) (route.Signal, error) {
	return f(ctx, prompt)
}

func TestEngineOverlongPromptSkipsAndLogsHashOnly(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	classifier := classifierFunc(func(context.Context, string) (route.Signal, error) {
		calls.Add(1)
		return route.Signal{}, errors.New("must not run")
	})
	dir := t.TempDir()
	engine := route.NewEngine(classifier, dir, 10)
	prompt := strings.Repeat("x", route.MaxPromptTokens*4+1)
	d := engine.Decide(context.Background(), "main", "sess-1", prompt, route.ModeShadow)
	if calls.Load() != 0 {
		t.Fatalf("classifier called %d times", calls.Load())
	}
	if d.Status != route.StatusSkipped || d.EstTokens != route.MaxPromptTokens+1 {
		t.Fatalf("decision = %+v", d)
	}
	if d.PromptHash != route.HashPrompt(prompt) || len(d.PromptHash) != 64 {
		t.Fatalf("prompt hash = %q", d.PromptHash)
	}

	data, err := os.ReadFile(filepath.Join(dir, route.DecisionLogName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(prompt)) {
		t.Fatal("decision log contains prompt plaintext")
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("JSONL lines = %d", len(lines))
	}
	var fields map[string]any
	if err := json.Unmarshal(lines[0], &fields); err != nil {
		t.Fatalf("decode JSONL: %v", err)
	}
	if _, ok := fields["prompt"]; ok {
		t.Fatal("decision JSON must not have a prompt field")
	}
	if _, ok := fields["outcome_hint"]; !ok {
		t.Fatal("decision JSON omitted outcome_hint placeholder")
	}
	if _, ok := fields["signal"]; ok {
		t.Fatal("decision signal must be flattened")
	}
}

func TestEngineRecordsClassifierFailure(t *testing.T) {
	t.Parallel()
	classifier := classifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Raw: "malformed", Latency: 12 * time.Millisecond}, errors.New("route: parse: malformed")
	})
	dir := t.TempDir()
	engine := route.NewEngine(classifier, dir, 10)
	d := engine.Decide(context.Background(), "main", "sess", "hello", route.ModeShadow)
	if d.Status != route.StatusError || d.Raw != "malformed" || d.LatencyMS != 12 {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(d.Error, "route: parse:") {
		t.Fatalf("error = %q", d.Error)
	}
	recent := engine.Recent(1)
	if len(recent) != 1 || recent[0] != d {
		t.Fatalf("recent = %+v, want %+v", recent, d)
	}
	if _, err := os.Stat(engine.Path()); err != nil {
		t.Fatalf("classifier failure was not logged: %v", err)
	}
}

func TestEngineLogFailureStaysInMemory(t *testing.T) {
	t.Parallel()
	classifier := classifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Domain: "General", Complexity: 1, Label: "small"}, nil
	})
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := route.NewEngine(classifier, filepath.Join(blocker, "child"), 10)
	var logged bytes.Buffer
	engine.SetLog(slog.New(slog.NewTextHandler(&logged, nil)))
	d := engine.Decide(context.Background(), "main", "sess", "hello", route.ModeShadow)
	if d.Status != route.StatusOK || d.Error != "" {
		t.Fatalf("decision = %+v", d)
	}
	if got := engine.Recent(1); len(got) != 1 || got[0] != d {
		t.Fatalf("history = %+v", got)
	}
	if !strings.Contains(logged.String(), "decision log append failed") {
		t.Fatalf("the failed write was not reported: %q", logged.String())
	}
}

func TestEngineProbeDoesNotRecord(t *testing.T) {
	t.Parallel()
	var gotPrompt string
	classifier := classifierFunc(func(_ context.Context, prompt string) (route.Signal, error) {
		gotPrompt = prompt
		return route.Signal{Domain: "Programming", Complexity: 3, Code: true, Label: "big"}, nil
	})
	engine := route.NewEngine(classifier, t.TempDir(), 10)
	sig, err := engine.Probe(context.Background())
	if err != nil || sig.Label != "big" || gotPrompt == "" {
		t.Fatalf("Probe = %+v, %v, prompt=%q", sig, err, gotPrompt)
	}
	if got := engine.Recent(0); len(got) != 0 {
		t.Fatalf("probe entered history: %+v", got)
	}
	if _, err := os.Stat(engine.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe created decision log: %v", err)
	}
}

func TestEngineHistoryIsBoundedNewestFirst(t *testing.T) {
	t.Parallel()
	classifier := classifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Domain: "General", Complexity: 1, Label: "small"}, nil
	})
	engine := route.NewEngine(classifier, t.TempDir(), 2)
	for _, prompt := range []string{"one", "two", "three"} {
		engine.Decide(context.Background(), "main", "sess", prompt, route.ModeShadow)
	}
	got := engine.Recent(0)
	if len(got) != 2 {
		t.Fatalf("history len = %d, want 2", len(got))
	}
	if got[0].PromptHash != route.HashPrompt("three") || got[1].PromptHash != route.HashPrompt("two") {
		t.Fatalf("newest history = %+v", got)
	}
	second, ok := engine.NthRecent(2)
	if !ok || second.PromptHash != route.HashPrompt("two") {
		t.Fatalf("NthRecent(2) = %+v, %v", second, ok)
	}
	if _, ok := engine.NthRecent(3); ok {
		t.Fatal("NthRecent(3) unexpectedly found evicted decision")
	}
}

func TestDecisionTagsAndSummary(t *testing.T) {
	t.Parallel()
	d := route.Decision{
		Mode: route.ModeShadow, Status: route.StatusOK, Label: "big",
		Complexity: 4, Domain: "Programming", LatencyMS: 17,
	}
	wantTags := []string{"route:mode=shadow", "route:label=big", "route:complexity=4"}
	gotTags := d.Tags()
	if strings.Join(gotTags, ",") != strings.Join(wantTags, ",") {
		t.Fatalf("Tags = %v, want %v", gotTags, wantTags)
	}
	for _, want := range []string{"shadow", "big", "complexity 4", "Programming", "17ms"} {
		if !strings.Contains(d.Summary(), want) {
			t.Errorf("Summary %q missing %q", d.Summary(), want)
		}
	}
}
