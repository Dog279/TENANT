package main

import (
	"context"
	"slices"
	"testing"

	"tenant/internal/approval"
	"tenant/internal/control"
	githubp "tenant/internal/plugins/github"
)

// A merge files under destructive but is explicit-only on the real broker: a
// destructive allow given for something else still asks (TEN-432).
func TestGitHubMergeApprovalIsExplicitOnly(t *testing.T) {
	if got := categorize(githubp.ActionMerge); got != catDestructive {
		t.Fatalf("github_merge files under %q, want %q", got, catDestructive)
	}
	b := approval.NewBroker(nil, categorize)
	b.SetRules(approval.NewRules())
	b.SetPermission(catDestructive, "allow")
	asked := 0
	b.SetAsk(func(context.Context, control.ApprovalRequest) control.ApprovalDecision {
		asked++
		return control.DenyOnce
	})
	if b.Confirm(context.Background(), githubp.ActionMerge, "merge #1") || asked != 1 {
		t.Fatalf("a destructive allow approved a merge (asked %d)", asked)
	}
	if !b.Confirm(context.Background(), "github_write", "comment") {
		t.Fatal("the destructive allow stopped covering ordinary GitHub writes")
	}
}

// Forgetting GitHub removes its merge rules with its token.
func TestGitHubForgetIncludesMergeRules(t *testing.T) {
	files := githubPlugin().Forget("cfg", "data")
	if !slices.Contains(files, githubp.MergeConfigPath("cfg")) || !slices.Contains(files, githubp.TokenPath("cfg")) {
		t.Fatalf("Forget = %v", files)
	}
}
