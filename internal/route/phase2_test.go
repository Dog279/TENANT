package route_test

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/model"
	"tenant/internal/route"
)

func TestEngineDecideWithPolicyLongPromptSkipsClassifier(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		calls.Add(1)
		return route.Signal{}, errors.New("must not classify a long prompt")
	}), t.TempDir(), 10)
	in := basePolicyInput()
	in.EstTokens = route.LongPromptTokens + 1
	d, err := engine.DecideWithPolicy(context.Background(), route.PolicyDecisionRequest{
		AgentID: "main", SessionID: "s", Prompt: "hash me", Mode: route.ModeActive, Input: in,
	})
	if err != nil {
		t.Fatalf("DecideWithPolicy: %v", err)
	}
	if calls.Load() != 0 || d.ClassifierAttempted {
		t.Fatalf("classifier calls/attempted = %d/%t, want 0/false", calls.Load(), d.ClassifierAttempted)
	}
	if d.SelectedTier != route.TierBig || d.PolicyReason != string(route.ReasonLongPrompt) || d.Status != route.StatusOK {
		t.Fatalf("decision = %+v", d)
	}
}

func TestEngineRefreshesPrivacyTargetsAfterClassification(t *testing.T) {
	t.Parallel()
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Label: "big", Complexity: 5}, nil
	}), t.TempDir(), 10)
	in := basePolicyInput()
	in.NeverCloud = true
	var refreshes atomic.Int32
	d, err := engine.DecideWithPolicy(context.Background(), route.PolicyDecisionRequest{
		Prompt: "short prompt", Mode: route.ModeActive, Input: in,
		RefreshInput: func(in *route.PolicyInput) {
			if refreshes.Add(1) == 2 {
				in.Big.Local = false
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("target refreshes = %d, want 2", got)
	}
	if d.SelectedTier != route.TierSmall || d.PolicyReason != string(route.ReasonNeverCloud) {
		t.Fatalf("decision = %+v", d)
	}
}

func TestEngineDeterministicOverrideAndStickySkipClassifier(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		calls.Add(1)
		return route.Signal{}, errors.New("classifier unavailable")
	}), t.TempDir(), 10)
	for _, mutate := range []func(*route.PolicyInput){
		func(in *route.PolicyInput) { in.Override = route.OverrideBig },
		func(in *route.PolicyInput) {
			in.StickySession, in.StickyTier = true, route.TierBig
		},
	} {
		in := basePolicyInput()
		mutate(&in)
		d, err := engine.DecideWithPolicy(context.Background(), route.PolicyDecisionRequest{
			Prompt: "short", Mode: route.ModeActive, Input: in,
		})
		if err != nil || d.SelectedTier != route.TierBig || d.ClassifierAttempted {
			t.Fatalf("deterministic decision/error = %+v / %v", d, err)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("classifier calls = %d, want 0", got)
	}
}

func TestEngineClassifierPanicAndLateSuccessTripBreaker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fn   classifierFunc
	}{
		{name: "panic", fn: func(context.Context, string) (route.Signal, error) { panic("boom") }},
		// A success that comes back after the deadline. It waits for the
		// deadline itself: a fixed sleep raced the 1ms timer, and on a loaded
		// Windows runner the sleep won and the success was on time.
		{name: "late success", fn: func(ctx context.Context, _ string) (route.Signal, error) {
			<-ctx.Done()
			return route.Signal{Label: "big", Complexity: 5}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := route.NewEngine(tc.fn, t.TempDir(), 10)
			request := route.PolicyDecisionRequest{
				Prompt: "short", Mode: route.ModeActive, Input: basePolicyInput(),
			}
			for i := 0; i < 3; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				_, err := engine.DecideWithPolicy(ctx, request)
				cancel()
				if err == nil {
					t.Fatalf("failure %d returned nil error", i+1)
				}
			}
			d, err := engine.DecideWithPolicy(context.Background(), request)
			if err != nil || d.PolicyReason != string(route.ReasonBreakerOpen) || d.Status != route.StatusSkipped {
				t.Fatalf("open decision/error = %+v / %v", d, err)
			}
		})
	}
}

func TestEngineBreakerTripsAndSkipsDeadClassifier(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		calls.Add(1)
		return route.Signal{Raw: "bad", Latency: 7 * time.Millisecond}, errors.New("route: parse: malformed")
	}), t.TempDir(), 10)
	request := route.PolicyDecisionRequest{
		AgentID: "main", SessionID: "s", Prompt: "short", Mode: route.ModeActive,
		Input: basePolicyInput(),
	}
	for i := 1; i <= 3; i++ {
		d, err := engine.DecideWithPolicy(context.Background(), request)
		if err == nil || d.Status != route.StatusError || d.SelectedTier != route.TierSmall {
			t.Fatalf("failure %d decision/error = %+v / %v", i, d, err)
		}
		if got := d.BreakerTripped; got != (i == 3) {
			t.Fatalf("failure %d BreakerTripped = %t", i, got)
		}
	}
	d, err := engine.DecideWithPolicy(context.Background(), request)
	if err != nil {
		t.Fatalf("open breaker should degrade without an error: %v", err)
	}
	if calls.Load() != 3 || d.Status != route.StatusSkipped ||
		d.PolicyReason != string(route.ReasonBreakerOpen) || d.BreakerState != route.BreakerOpen {
		t.Fatalf("open decision/calls = %+v / %d", d, calls.Load())
	}
	stats := engine.Stats()
	if stats.BreakerTrips != 1 || stats.ClassifierAttempts != 3 || stats.ParseFailures != 3 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestEngineNamedForcePierceIsAudited(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	dir := t.TempDir()
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		calls.Add(1)
		return route.Signal{}, nil
	}), dir, 10)
	force := route.Target{
		Tier: route.TierBig, Role: model.Role("planner-force"),
		ProfileID: "cloud-explicit", Local: false, OperationalBudget: 2000,
	}
	in := basePolicyInput()
	in.NeverCloud = true
	in.Force = &force
	d, err := engine.DecideWithPolicy(context.Background(), route.PolicyDecisionRequest{
		AgentID: "main", SessionID: "s", Prompt: "private", Input: in,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || !d.PrivacyPierced || d.SelectedProfileID != "cloud-explicit" ||
		d.PolicyReason != string(route.ReasonForceProfile) {
		t.Fatalf("decision/calls = %+v / %d", d, calls.Load())
	}
	data, err := os.ReadFile(engine.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"privacy_pierced":true`, `"selected_profile_id":"cloud-explicit"`, `"policy_reason":"force_profile"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("decision log missing %s: %s", want, data)
		}
	}
	if strings.Contains(string(data), "private") {
		t.Fatal("decision log exposed prompt plaintext")
	}
}

func TestEngineStatsUsesDurableDecisionLog(t *testing.T) {
	t.Parallel()
	type outcome struct {
		signal route.Signal
		err    error
	}
	outcomes := []outcome{
		{signal: route.Signal{Label: "big", Complexity: 5, Latency: 10 * time.Millisecond}},
		{signal: route.Signal{Label: "big", Complexity: 2, Latency: 20 * time.Millisecond}},
		{signal: route.Signal{Raw: "bad", Latency: 30 * time.Millisecond}, err: errors.New("route: parse: malformed")},
		{signal: route.Signal{Label: "small", Complexity: 1, Latency: 40 * time.Millisecond}},
		{signal: route.Signal{Label: "big", Complexity: 5, Latency: 50 * time.Millisecond}},
	}
	var next atomic.Int32
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		i := int(next.Add(1)) - 1
		return outcomes[i].signal, outcomes[i].err
	}), t.TempDir(), 1) // prove Stats reads durable history, not this 1-entry cache
	for range outcomes {
		_, _ = engine.DecideWithPolicy(context.Background(), route.PolicyDecisionRequest{
			AgentID: "main", SessionID: "s", Prompt: "p", Mode: route.ModeShadow,
			Input: basePolicyInput(),
		})
	}
	if got := len(engine.Recent(0)); got != 1 {
		t.Fatalf("bounded memory len = %d, want 1", got)
	}
	stats := engine.Stats()
	if stats.TotalDecisions != 5 || stats.RoutedDecisions != 5 || stats.BigDecisions != 2 {
		t.Fatalf("decision counts = %+v", stats)
	}
	if math.Abs(stats.BigRate-0.4) > 1e-9 {
		t.Errorf("BigRate = %v, want .4", stats.BigRate)
	}
	if stats.ShadowCompared != 4 || stats.ShadowDisagreements != 1 ||
		math.Abs(stats.ShadowDisagreementRate-0.25) > 1e-9 {
		t.Errorf("shadow stats = %+v", stats)
	}
	if stats.ClassifierAttempts != 5 || stats.ParseFailures != 1 ||
		math.Abs(stats.ParseFailureRate-0.2) > 1e-9 {
		t.Errorf("parse stats = %+v", stats)
	}
	if stats.P50ClassifyLatency != 30*time.Millisecond || stats.P95ClassifyLatency != 50*time.Millisecond {
		t.Errorf("latency stats = %v/%v, want 30ms/50ms", stats.P50ClassifyLatency, stats.P95ClassifyLatency)
	}
}

func TestEngineStatsFallsBackToMemoryWhenLogIsBroken(t *testing.T) {
	t.Parallel()
	blocker := t.TempDir() + string(os.PathSeparator) + "file"
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Label: "big", Complexity: 5, Latency: time.Millisecond}, nil
	}), blocker+string(os.PathSeparator)+"child", 10)
	_, err := engine.DecideWithPolicy(context.Background(), route.PolicyDecisionRequest{
		Prompt: "p", Mode: route.ModeShadow, Input: basePolicyInput(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stats := engine.Stats()
	if stats.TotalDecisions != 1 || stats.BigDecisions != 1 || stats.ClassifierAttempts != 1 {
		t.Fatalf("memory fallback stats = %+v", stats)
	}
}

func TestEngineStatsKeepsValidDurablePrefixWithTruncatedTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	engine := route.NewEngine(classifierFunc(func(context.Context, string) (route.Signal, error) {
		return route.Signal{Label: "big", Complexity: 5, Latency: 9 * time.Millisecond}, nil
	}), dir, 10)
	if _, err := engine.DecideWithPolicy(context.Background(), route.PolicyDecisionRequest{
		Prompt: "p", Mode: route.ModeActive, Input: basePolicyInput(),
	}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(engine.Path(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// A fresh CLI-style engine has no in-memory fallback; it must still report
	// the complete JSONL records before the interrupted final append.
	fresh := route.NewEngine(nil, dir, 10)
	stats := fresh.Stats()
	if stats.TotalDecisions != 1 || stats.BigDecisions != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}
