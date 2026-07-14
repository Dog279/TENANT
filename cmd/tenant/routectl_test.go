package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/route"
)

func TestCmdRouteModePersistence(t *testing.T) {
	tests := []struct {
		command     string
		wantEnabled bool
		wantMode    string
		wantAssist  bool
	}{
		{command: "on", wantEnabled: true, wantMode: route.ModeActive},
		{command: "off", wantEnabled: false, wantMode: route.ModeActive},
		{command: "shadow", wantEnabled: true, wantMode: route.ModeShadow},
		{command: "assisted", wantEnabled: true, wantMode: route.ModeShadow, wantAssist: true},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			cfgDir, dataDir := t.TempDir(), t.TempDir()
			lc := routeCLITestConfig()
			lc.Route.Mode = route.ModeActive
			lc.Route.ForceProfile = "big/vllm-planner"
			if err := lc.save(cfgDir); err != nil {
				t.Fatal(err)
			}

			_, err := captureRouteCLIOutput(t, func() error {
				return cmdRoute(context.Background(), []string{tt.command, "--config", cfgDir, "--data", dataDir})
			})
			if err != nil {
				t.Fatalf("cmdRoute(%s): %v", tt.command, err)
			}
			got, err := loadLaunchConfig(cfgDir)
			if err != nil {
				t.Fatal(err)
			}
			if got.Route == nil || got.Route.Enabled != tt.wantEnabled || got.Route.Mode != tt.wantMode || got.Route.Assist != tt.wantAssist {
				t.Fatalf("persisted route = %+v, want enabled=%t mode=%s assist=%t", got.Route, tt.wantEnabled, tt.wantMode, tt.wantAssist)
			}
			if got.Route.ForceProfile != "" {
				t.Fatalf("force_profile = %q, want cleared", got.Route.ForceProfile)
			}
		})
	}
}

func TestCmdRouteEverywhereRequiresPassingGraduationGate(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	lc := routeCLITestConfig()
	lc.Route.Mode = route.ModeActive
	if err := lc.save(cfgDir); err != nil {
		t.Fatal(err)
	}
	_, err := captureRouteCLIOutput(t, func() error {
		return cmdRoute(context.Background(), []string{"everywhere", "--config", cfgDir, "--data", dataDir})
	})
	if err == nil || !strings.Contains(err.Error(), "graduation gate FAIL") {
		t.Fatalf("everywhere activation error = %v", err)
	}
	got, loadErr := loadLaunchConfig(cfgDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got.Route.Mode != route.ModeActive {
		t.Fatalf("failed graduation changed mode to %q", got.Route.Mode)
	}
}

func TestCmdRouteOnRetainsActionableBigRoleValidation(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	lc := &launchConfig{
		Provider:  "small",
		Providers: map[string]*providerConfig{"small": {Kind: "vllm"}},
		Route:     &routeConfig{Enabled: true, Mode: route.ModeShadow},
	}
	if err := lc.save(cfgDir); err != nil {
		t.Fatal(err)
	}

	_, err := captureRouteCLIOutput(t, func() error {
		return cmdRoute(context.Background(), []string{"on", "--config", cfgDir, "--data", dataDir})
	})
	if err == nil {
		t.Fatal("cmdRoute(on) error = nil, want missing big-role rejection")
	}
	for _, want := range []string{"route.big_role", "/model add-cloud"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	got, loadErr := loadLaunchConfig(cfgDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got.Route.Mode != route.ModeShadow {
		t.Fatalf("failed activation persisted mode %q, want shadow", got.Route.Mode)
	}
}

func TestCmdRouteForceOfflineValidationAndPersistence(t *testing.T) {
	accepted := []string{
		"vllm-planner",             // active provider
		"big/anthropic-planner",    // planner-big binding
		"fallback/vllm-executor",   // configured fallback
		"fallback/vllm-summarizer", // configured fallback
	}
	for _, profileID := range accepted {
		t.Run(profileID, func(t *testing.T) {
			cfgDir, dataDir := t.TempDir(), t.TempDir()
			lc := routeCLITestConfig()
			if err := lc.save(cfgDir); err != nil {
				t.Fatal(err)
			}

			out, err := captureRouteCLIOutput(t, func() error {
				return cmdRoute(context.Background(), []string{"force", profileID, "--config", cfgDir, "--data", dataDir})
			})
			if err != nil {
				t.Fatalf("cmdRoute(force %s): %v", profileID, err)
			}
			if !strings.Contains(out, "WARNING") || !strings.Contains(out, "pierces never_cloud") {
				t.Errorf("force output = %q, want loud privacy warning", out)
			}
			got, err := loadLaunchConfig(cfgDir)
			if err != nil {
				t.Fatal(err)
			}
			wantProfileID := profileID
			if profileID == "vllm-planner" {
				wantProfileID = "small/vllm-planner"
			}
			if !got.Route.Enabled || got.Route.Mode != route.ModeActive || got.Route.ForceProfile != wantProfileID {
				t.Fatalf("persisted force = %+v", got.Route)
			}
		})
	}

	cfgDir, dataDir := t.TempDir(), t.TempDir()
	if err := routeCLITestConfig().save(cfgDir); err != nil {
		t.Fatal(err)
	}
	_, err := captureRouteCLIOutput(t, func() error {
		return cmdRoute(context.Background(), []string{"force", "big/not-a-profile", "--config", cfgDir, "--data", dataDir})
	})
	if err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("unknown force error = %v", err)
	}
}

func TestCmdRouteForceStillRequiresBigRole(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	lc := &launchConfig{
		Provider:  "small",
		Providers: map[string]*providerConfig{"small": {Kind: "vllm"}},
		Route:     &routeConfig{Mode: route.ModeShadow},
	}
	if err := lc.save(cfgDir); err != nil {
		t.Fatal(err)
	}

	_, err := captureRouteCLIOutput(t, func() error {
		return cmdRoute(context.Background(), []string{"force", "vllm-planner", "--config", cfgDir, "--data", dataDir})
	})
	if err == nil || !strings.Contains(err.Error(), "route.big_role") {
		t.Fatalf("force without big_role error = %v", err)
	}
}

func TestCmdRouteStatusAndDurableStats(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	if err := (&launchConfig{}).save(cfgDir); err != nil {
		t.Fatal(err)
	}

	status, err := captureRouteCLIOutput(t, func() error {
		return cmdRoute(context.Background(), []string{"status", "--config", cfgDir, "--data", dataDir})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "Prompt router") || !strings.Contains(status, "enabled: false") {
		t.Fatalf("status output = %q", status)
	}

	d := route.Decision{
		Timestamp: time.Now().UTC(), Mode: route.ModeActive, Status: route.StatusOK,
		Label: "big", SelectedTier: route.TierBig, ClassifierAttempted: true, LatencyMS: 17,
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, route.DecisionLogName), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err := captureRouteCLIOutput(t, func() error {
		return cmdRoute(context.Background(), []string{"stats", "--config", cfgDir, "--data", dataDir})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"decisions: 1", "big rate: 100.0%", "17ms"} {
		if !strings.Contains(stats, want) {
			t.Errorf("stats output %q missing %q", stats, want)
		}
	}
}

func TestCmdRouteRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"force"},
		{"force", "vllm-planner", "extra"},
		{"status", "extra"},
		{"frobnicate"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := captureRouteCLIOutput(t, func() error {
				return cmdRoute(context.Background(), args)
			})
			if err == nil || !strings.Contains(err.Error(), "usage: tenant route") {
				t.Fatalf("cmdRoute(%v) error = %v", args, err)
			}
		})
	}
}

func routeCLITestConfig() *launchConfig {
	return &launchConfig{
		Provider: "small",
		Providers: map[string]*providerConfig{
			"small":    {Kind: "vllm", Endpoint: "http://localhost:8000", Model: "small"},
			"big":      {Kind: "anthropic", Model: "big"},
			"fallback": {Kind: "vllm", Endpoint: "http://localhost:9000", Model: "fallback"},
		},
		Fallbacks: []string{"fallback"},
		Route: &routeConfig{
			Enabled: true, Mode: route.ModeShadow, BigRole: "big",
			Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"},
		},
	}
}

func captureRouteCLIOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	callErr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, readErr := io.ReadAll(r)
	if closeErr := r.Close(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(b), callErr
}
