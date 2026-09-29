package github

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMergeLive runs the gate against real GitHub, which the fake can't
// stand in for: the sha guard, pagination and filter=latest as GitHub serves
// them. Opt-in and destructive (it merges a PR), so it runs only on a
// throwaway repo the operator names:
//
//	TENANT_GITHUB_LIVE=1
//	TENANT_GITHUB_LIVE_REPO=owner/scratch   a repo you don't mind merging into
//	TENANT_GITHUB_LIVE_TOKEN=...            a PAT scoped to that repo only
//	TENANT_GITHUB_LIVE_PR=<n>               an open PR on it whose CI is green
//	TENANT_GITHUB_LIVE_CHECK=<name>         the required check run's name
//
// The not-yet-built rules (TEN-433/434) are passed for the run.
func TestMergeLive(t *testing.T) {
	if os.Getenv("TENANT_GITHUB_LIVE") != "1" {
		t.Skip("set TENANT_GITHUB_LIVE=1 and the TENANT_GITHUB_LIVE_* variables to run against real GitHub")
	}
	repo, tok := os.Getenv("TENANT_GITHUB_LIVE_REPO"), os.Getenv("TENANT_GITHUB_LIVE_TOKEN")
	check := os.Getenv("TENANT_GITHUB_LIVE_CHECK")
	n, err := strconv.Atoi(os.Getenv("TENANT_GITHUB_LIVE_PR"))
	if err != nil || repo == "" || tok == "" || check == "" {
		t.Fatal("TENANT_GITHUB_LIVE_REPO, _TOKEN, _PR and _CHECK are all required")
	}
	passUnbuilt(t)
	cfg := filepath.Join(t.TempDir(), MergeConfigFile)
	if err := os.WriteFile(cfg, []byte(`{"`+repo+`": {"required_checks": [{"name": "`+check+`"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := Open(Config{Repos: []string{repo + ":write"}, Token: tok, MergeConfigPath: cfg})
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	d := NewDispatcher(svc, Policy{Confirm: func(context.Context, string, string) bool { asked++; return true }})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, isErr, _ := d.Dispatch(ctx, call("github_merge_readiness", map[string]any{"number": n}))
	if isErr || !strings.Contains(out, "ready to merge") {
		t.Fatalf("readiness on a green PR: %s", out)
	}
	rep, err := d.evaluate(ctx, svc.Primary(), n)
	if err != nil {
		t.Fatal(err)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("github_merge", map[string]any{"number": n, "expected_sha": strings.Repeat("0", 40)})); !isErr || !strings.Contains(out, "moved") || asked != 0 {
		t.Fatalf("a wrong expected_sha: isErr=%v asked=%d out=%s", isErr, asked, out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("github_merge", map[string]any{"number": n, "expected_sha": rep.HeadSHA}))
	if isErr || !strings.Contains(out, "merged") || asked != 1 {
		t.Fatalf("merge: isErr=%v asked=%d out=%s", isErr, asked, out)
	}
}
