package main

// research_dashctl.go: the dashboard's Research page (TEN-210). The terminal
// app shares its researchControl; serve gets a research-only runtime copied
// from its team config. WP-research fills both in.

import (
	"context"

	"tenant/internal/dashboard"
	"tenant/internal/lifecycle"
)

// newDashResearch adapts the terminal app's rc; next streams progress lines
// to its feed. nil (an untyped nil interface) leaves the page showing
// "isn't available".
func newDashResearch(base context.Context, rc *researchControl, web bool, next func(string, ...any)) dashboard.ResearchControl {
	return nil
}

// newServeDashResearch builds serve's research: its own runtime from cfg,
// sharing shared's tools. Web browsing goes off when cfg.WebConfig.NoSandbox
// and this process runs as a Windows service account (D19).
func newServeDashResearch(base context.Context, life *lifecycle.Group, cfg TeamConfig, shared *TeamRuntime,
	rc researchControl, dataDir string, web bool) dashboard.ResearchControl {
	return nil
}
