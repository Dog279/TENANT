package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// TEN-358: config.json `health_gating` reaches the router, and doctor and
// /model fallback report what it will do.

func TestHealthGatingConfigModelConfig(t *testing.T) {
	tests := []struct {
		name string
		in   *healthGatingConfig
		want model.HealthConfig
	}{
		{name: "absent", in: nil, want: model.HealthConfig{}},
		{name: "zero threshold is off", in: &healthGatingConfig{MinSamples: 3}, want: model.HealthConfig{}},
		{name: "negative threshold is off", in: &healthGatingConfig{SlowThresholdMS: -1}, want: model.HealthConfig{}},
		{
			name: "threshold only",
			in:   &healthGatingConfig{SlowThresholdMS: 20000},
			want: model.HealthConfig{SlowThreshold: 20 * time.Second},
		},
		{
			name: "every field",
			in:   &healthGatingConfig{SlowThresholdMS: 1500, MinSamples: 3, Alpha: 0.5, CooldownMS: 60000},
			want: model.HealthConfig{SlowThreshold: 1500 * time.Millisecond, MinSamples: 3, Alpha: 0.5, Cooldown: time.Minute},
		},
		{
			name: "negative cooldown leaves the default",
			in:   &healthGatingConfig{SlowThresholdMS: 1000, CooldownMS: -5},
			want: model.HealthConfig{SlowThreshold: time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.ModelConfig(); got != tt.want {
				t.Fatalf("modelConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDescribeHealthGating(t *testing.T) {
	if got := describeHealthGating(model.HealthConfig{}); got != "off" {
		t.Errorf("zero config = %q, want off", got)
	}
	got := describeHealthGating(model.HealthConfig{SlowThreshold: 20 * time.Second})
	for _, want := range []string{"over 20s", "after 5 calls", "skipped for 30s"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q should contain %q (engine defaults filled in)", got, want)
		}
	}
}

func TestInstallFallbackChainAppliesHealthGating(t *testing.T) {
	lc := &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small":  {Kind: "vllm", Endpoint: "http://small.local:8000", Model: "small-model"},
			"backup": {Kind: "vllm", Endpoint: "http://backup.local:8000", Model: "backup-model"},
		},
		Fallbacks:    []string{"backup"},
		HealthGating: &healthGatingConfig{SlowThresholdMS: 20000, MinSamples: 3},
	}
	r := model.NewRouter(model.NewEmptyRegistry(), testRouteBindingLogger())
	installFallbackChain(r, t.TempDir(), lc, 0, testRouteBindingLogger())
	want := model.HealthConfig{SlowThreshold: 20 * time.Second, MinSamples: 3}
	if got := r.HealthGating(); got != want {
		t.Fatalf("router gate = %+v, want %+v", got, want)
	}

	// A /model swap re-installs the chain, so removing the setting turns the
	// gate off without a restart.
	lc.HealthGating = nil
	installFallbackChain(r, t.TempDir(), lc, 0, testRouteBindingLogger())
	if got := r.HealthGating(); got != (model.HealthConfig{}) {
		t.Fatalf("router gate = %+v after removing health_gating, want off", got)
	}
}

func TestDoctor_CheckHealthGating(t *testing.T) {
	providers := map[string]*providerConfig{
		"small":  {Kind: "vllm", Endpoint: "http://small", Model: "m"},
		"backup": {Kind: "vllm", Endpoint: "http://backup", Model: "m"},
	}
	on := &healthGatingConfig{SlowThresholdMS: 20000}
	cases := []struct {
		name       string
		lc         *launchConfig
		wantStatus checkStatus
		mustDetail []string
	}{
		{
			name:       "no launch config",
			wantStatus: statusSkip,
			mustDetail: []string{"unavailable"},
		},
		{
			name:       "off",
			lc:         &launchConfig{Provider: "small", Providers: providers, Fallbacks: []string{"backup"}},
			wantStatus: statusSkip,
			mustDetail: []string{"off", "health_gating.slow_threshold_ms"},
		},
		{
			name:       "on without a chain",
			lc:         &launchConfig{Provider: "small", Providers: providers, HealthGating: on},
			wantStatus: statusWarn,
			mustDetail: []string{"nothing to move to"},
		},
		{
			name:       "chain names only the active and an unknown provider",
			lc:         &launchConfig{Provider: "small", Providers: providers, Fallbacks: []string{"small", "ghost"}, HealthGating: on},
			wantStatus: statusWarn,
			mustDetail: []string{"nothing to move to"},
		},
		{
			name:       "on with a chain",
			lc:         &launchConfig{Provider: "small", Providers: providers, Fallbacks: []string{"backup"}, HealthGating: on},
			wantStatus: statusOK,
			mustDetail: []string{"over 20s", "after 5 calls", "skipped for 30s", "small → backup"},
		},
		{
			name: "values the engine replaces",
			lc: &launchConfig{Provider: "small", Providers: providers, Fallbacks: []string{"backup"},
				HealthGating: &healthGatingConfig{SlowThresholdMS: 20000, MinSamples: -2, Alpha: 1.5, CooldownMS: -1}},
			wantStatus: statusWarn,
			mustDetail: []string{"min_samples -2", "alpha 1.5", "cooldown_ms -1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := checkHealthGating(context.Background(), &doctorEnv{lc: c.lc})
			if r.Status != c.wantStatus {
				t.Errorf("status = %v, want %v (detail=%q)", r.Status, c.wantStatus, r.Detail)
			}
			for _, want := range c.mustDetail {
				if !strings.Contains(r.Detail, want) {
					t.Errorf("detail %q should contain %q", r.Detail, want)
				}
			}
		})
	}
}
