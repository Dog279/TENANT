package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"tenant/internal/model"
)

// openTest builds a Service pointed at an httptest server.
func openTest(t *testing.T, srv *httptest.Server) *Service {
	t.Helper()
	svc, err := Open(Config{
		Repos:      []string{"Dog279/TENANT:write"},
		Token:      "ghp_testtoken0000000000000000000000000000",
		APIBase:    srv.URL + "/",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return svc
}

func call(name string, args any) model.ToolCall {
	b, _ := json.Marshal(args)
	return model.ToolCall{Name: name, Arguments: b}
}

// Every write/destructive tool must be Gated so the toolmux surfaces it as
// destructive and the cron read/comms-safe surface strips it. Reads must NOT be
// gated, or an unattended job loses the ability to report status.
func TestTools_GatingShape(t *testing.T) {
	tools := NewDispatcher(nil, Policy{}).Tools()

	wantGated := map[string]bool{
		"github_create_branch": true, "github_create_issue": true, "github_comment": true,
		"github_create_pr": true, "github_update_pr": true, "github_delete_branch": true,
		"github_merge": true,
	}
	wantUngated := map[string]bool{
		"github_changes": true, "github_search": true, "github_get_issue": true, "github_get_pr": true,
		"github_list_files": true, "github_read_file": true, "github_pr_checks": true,
		"github_check_logs": true, "github_merge_readiness": true,
	}

	seen := map[string]bool{}
	for _, tl := range tools {
		seen[tl.Name] = true
		switch {
		case wantGated[tl.Name] && !tl.Gated:
			t.Errorf("%s must be Gated", tl.Name)
		case wantUngated[tl.Name] && tl.Gated:
			t.Errorf("%s is a read and must NOT be Gated", tl.Name)
		case !wantGated[tl.Name] && !wantUngated[tl.Name]:
			t.Errorf("unexpected tool %q - add it to the gating table deliberately", tl.Name)
		}
		if len(tl.Parameters) == 0 {
			t.Errorf("%s has no parameter schema", tl.Name)
		}
	}
	for n := range wantGated {
		if !seen[n] {
			t.Errorf("missing tool %s", n)
		}
	}
	for n := range wantUngated {
		if !seen[n] {
			t.Errorf("missing tool %s", n)
		}
	}
}

// Tools that must NEVER be exposed, because exposing them at all would let the
// agent disable the safety around it (rewrite CI, read secrets, delete repo).
func TestTools_ForbiddenSurfaceAbsent(t *testing.T) {
	forbidden := []string{
		"github_delete_repo", "github_update_settings", "github_set_branch_protection",
		"github_create_secret", "github_read_secret", "github_update_workflow",
		"github_force_push", "github_push",
	}
	tools := NewDispatcher(nil, Policy{}).Tools()
	for _, f := range forbidden {
		for _, tl := range tools {
			if tl.Name == f {
				t.Errorf("%s must not be exposed", f)
			}
		}
	}
}

// Fail-closed: with a zero Policy (no AllowWrite, nil Confirm) every gated tool
// must refuse WITHOUT issuing a single HTTP request. If the gate ran after the
// call, the damage would already be done.
func TestDispatch_FailsClosedWithoutHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("gated tool reached the network: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewDispatcher(openTest(t, srv), Policy{}) // deny-by-default

	// Only tools whose gate needs no prior lookup. create_branch/delete_branch
	// resolve the default branch first, which is a legitimate read.
	cases := []model.ToolCall{
		call("github_create_issue", map[string]any{"title": "x"}),
		call("github_comment", map[string]any{"number": 1, "body": "x"}),
		call("github_update_pr", map[string]any{"number": 1, "title": "x"}),
	}
	for _, c := range cases {
		out, isErr, err := d.Dispatch(context.Background(), c)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.Name, err)
		}
		if !isErr || !strings.Contains(out, "blocked") {
			t.Errorf("%s should be blocked deny-by-default, got isErr=%v out=%q", c.Name, isErr, out)
		}
	}
}

// With GitHub unconfigured, a merge reports the setup step and goes nowhere,
// however permissive the policy.
func TestDispatch_MergeWithNilService(t *testing.T) {
	d := NewDispatcher(nil, Policy{AllowWrite: true, Confirm: func(context.Context, string, string) bool { return true }})
	out, isErr, _ := d.Dispatch(context.Background(), call("github_merge", map[string]any{"number": 1, "expected_sha": "abc"}))
	if !isErr || !strings.Contains(out, "not configured") {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
}

// Deleting the default branch is refused regardless of approval: an operator
// approving "delete a branch" is not approving the deletion of main.
func TestDispatch_DeleteDefaultBranchRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only a DELETE of the DEFAULT branch is a failure. The feature-branch
		// delete below is supposed to reach the network.
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "heads/main") {
			t.Errorf("default-branch delete reached the network: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	}))
	defer srv.Close()

	d := NewDispatcher(openTest(t, srv), Policy{
		AllowWrite: true,
		Confirm:    func(context.Context, string, string) bool { return true },
	})
	out, isErr, _ := d.Dispatch(context.Background(), call("github_delete_branch", map[string]any{"name": "main"}))
	if !isErr || !strings.Contains(out, "refused") {
		t.Fatalf("deleting the default branch must be refused, got isErr=%v out=%q", isErr, out)
	}

	// A feature branch with approval is allowed - proving the guard is targeted
	// at the default branch, not a blanket refusal that makes the tool useless.
	out, isErr, _ = d.Dispatch(context.Background(), call("github_delete_branch", map[string]any{"name": "feat/x"}))
	if isErr {
		t.Fatalf("approved feature-branch delete should succeed, got %q", out)
	}
}

// A read must work with the zero Policy - unattended jobs depend on it.
func TestDispatch_ReadHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/issues/7") {
			_, _ = w.Write([]byte(`{"number":7,"title":"Router flake","state":"open",
				"user":{"login":"Dog279"},"labels":[{"name":"bug"}],"body":"it flakes"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := NewDispatcher(openTest(t, srv), Policy{})
	out, isErr, err := d.Dispatch(context.Background(), call("github_get_issue", map[string]any{"number": 7}))
	if err != nil || isErr {
		t.Fatalf("read failed: isErr=%v err=%v out=%q", isErr, err, out)
	}
	for _, want := range []string{"#7", "Router flake", "open", "Dog279", "bug", "it flakes"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Searches must stay pinned to the configured repo even if the model supplies
// its own repo: qualifier - the pin is what keeps a single-repo PAT meaningful.
func TestDispatch_SearchIsPinnedToConfiguredRepo(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
	}))
	defer srv.Close()

	d := NewDispatcher(openTest(t, srv), Policy{})
	_, _, err := d.Dispatch(context.Background(), call("github_search",
		map[string]any{"query": "repo:someoneelse/private is:pr"}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.HasPrefix(gotQuery, "repo:Dog279/TENANT ") {
		t.Fatalf("search must be pinned to the configured repo, sent q=%q", gotQuery)
	}
}

// Open must refuse a malformed repo rather than guessing.
func TestOpen_RejectsBadRepoAndMissingToken(t *testing.T) {
	if _, err := Open(Config{Repos: []string{"not-a-slug"}, Token: "ghp_x"}); err == nil {
		t.Error("expected an error for a repo without owner/name")
	}
	if _, err := Open(Config{Repos: []string{"o/r"}}); err == nil {
		t.Error("expected an error when no token is configured")
	}
}

// Redaction is the last line of defence: go-github and git both echo the
// request URL on failure, and that URL can carry the token.
func TestRedact_ScrubsEveryTokenShape(t *testing.T) {
	cases := []string{
		"Get https://api.github.com/repos: bad credentials ghp_AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH",
		"remote: https://x-access-token:ghs_SECRETVALUE123456@github.com/o/r.git denied",
		"token github_pat_11ABCDEFG0123456789_abcdefghijklmnop rejected",
	}
	for _, in := range cases {
		out := Redact(in)
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("nothing redacted in %q -> %q", in, out)
		}
		for _, leak := range []string{"ghp_AAAA", "ghs_SECRET", "github_pat_11ABC", "SECRETVALUE"} {
			if strings.Contains(out, leak) {
				t.Errorf("token leaked through Redact: %q", out)
			}
		}
	}
}

// A dispatch error must never carry the token, even though the transport error
// contains the full request URL.
func TestDispatch_ErrorsAreRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credentials for ghp_LEAKEDTOKENVALUE000000000000000000", http.StatusUnauthorized)
	}))
	defer srv.Close()

	d := NewDispatcher(openTest(t, srv), Policy{})
	out, isErr, _ := d.Dispatch(context.Background(), call("github_get_issue", map[string]any{"number": 1}))
	if !isErr {
		t.Fatal("expected an error result")
	}
	if strings.Contains(out, "ghp_LEAKED") {
		t.Fatalf("token leaked into the tool output: %q", out)
	}
}

// Token resolution order, and the round-trip through the on-disk cache.
//
// NOTE: this deliberately does NOT assert 0600. Go's mode bits do not produce
// an owner-only DACL on Windows (the file lands 0666-equivalent), which is the
// tracked gap in TEN-325 and already makes the Atlassian and MCP token-store
// permission tests fail on this platform. Asserting it here would add a fourth
// knowingly-red test rather than catching a regression.
func TestToken_ResolutionOrderAndRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if got := ResolveToken("ghp_explicit", dir); got != "ghp_explicit" {
		t.Errorf("explicit token should win, got %q", got)
	}

	t.Setenv("GITHUB_TOKEN", "ghp_fromenv")
	if got := ResolveToken("", dir); got != "ghp_fromenv" {
		t.Errorf("env should be used when no explicit token, got %q", got)
	}

	if err := SaveToken(dir, "ghp_fromcache"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := os.Stat(TokenPath(dir)); err != nil {
		t.Fatalf("token cache not written: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_PAT", "")
	if got := ResolveToken("", dir); got != "ghp_fromcache" {
		t.Errorf("cache should be the fallback, got %q", got)
	}

	if err := SaveToken(dir, "   "); err == nil {
		t.Error("saving an empty token should fail loudly")
	}
}

// An unconfigured plugin must explain how to configure itself instead of
// panicking on a nil client.
func TestDispatch_NilServiceGivesSetupHint(t *testing.T) {
	d := NewDispatcher(nil, Policy{})
	out, isErr, err := d.Dispatch(context.Background(), call("github_search", map[string]any{"query": "is:pr"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isErr || !strings.Contains(out, "not configured") {
		t.Fatalf("expected a setup hint, got %q", out)
	}
}
