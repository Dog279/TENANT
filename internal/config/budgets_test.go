package config

import (
	"testing"
	"time"

	"tenant/internal/orchestra"
)

func TestLaunchConfigTeamBudgetsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := orchestra.Budgets{
		MaxConcurrentSubagents:  2,
		MaxAgentsPerRun:         7,
		MaxToolCallsPerTurn:     9,
		MaxToolCallsPerRun:      21,
		MaxTokensPerRun:         12_345,
		MaxCostUSD:              3.5,
		MaxWallTime:             17 * time.Second,
		MaxBrowserInstances:     1,
		BusHistoryCap:           44,
		InputCostPerMillionUSD:  1.25,
		OutputCostPerMillionUSD: 4.5,
	}
	if err := (&Launch{Budgets: want}).Save(dir); err != nil {
		t.Fatal(err)
	}
	lc, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := EffectiveBudgets(lc); got != want {
		t.Fatalf("budgets did not round-trip: got %+v want %+v", got, want)
	}
}
