package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// openMulti builds a service watching two repos: the primary is writable, the
// second is watch-only. That is the shape the operator asked for - every repo
// reports, only some are worked on autonomously.
func openMulti(t *testing.T, srv *httptest.Server) *Service {
	t.Helper()
	svc, err := Open(Config{
		Repos:      []string{"Dog279/TENANT:write", "Dog279/bitematch"},
		Token:      "ghp_testtoken0000000000000000000000000000",
		APIBase:    srv.URL + "/",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return svc
}

func TestParseRepoEntry_WriteSuffix(t *testing.T) {
	cases := []struct {
		in    string
		slug  string
		write bool
	}{
		{"Dog279/TENANT", "Dog279/TENANT", false},
		{"Dog279/TENANT:write", "Dog279/TENANT", true},
		{"Dog279/TENANT:rw", "Dog279/TENANT", true},
		{"Dog279/TENANT:read", "Dog279/TENANT", false},
		{"  Dog279/bitematch  ", "Dog279/bitematch", false},
	}
	for _, c := range cases {
		got, err := ParseRepoEntry(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got.Slug() != c.slug || got.Write != c.write {
			t.Errorf("%q -> %s write=%v, want %s write=%v", c.in, got.Slug(), got.Write, c.slug, c.write)
		}
	}
	if _, err := ParseRepoEntry("nope"); err == nil {
		t.Error("a bare name should be rejected")
	}
}

// The round trip through config must not silently downgrade a writable repo to
// read-only - that would be a safety setting quietly evaporating.
func TestParseRepos_RoundTripPreservesWrite(t *testing.T) {
	out, err := ParseRepos("Dog279/TENANT:write, Dog279/bitematch")
	if err != nil {
		t.Fatalf("ParseRepos: %v", err)
	}
	if len(out) != 2 || out[0] != "Dog279/TENANT:write" || out[1] != "Dog279/bitematch" {
		t.Fatalf("round trip lost the write flag: %v", out)
	}
	// Duplicates collapse.
	out, err = ParseRepos("a/b, a/b, c/d")
	if err != nil || len(out) != 2 {
		t.Fatalf("dedup failed: %v %v", out, err)
	}
}

// A repo outside the watch list is refused, even though the token may well have
// access to it. The config is the boundary, not the token's reach.
func TestResolve_RefusesUnwatchedRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unwatched repo reached the network: %s", r.URL.Path)
	}))
	defer srv.Close()

	d := NewDispatcher(openMulti(t, srv), Policy{AllowWrite: true})
	out, isErr, _ := d.Dispatch(context.Background(),
		call("github_get_issue", map[string]any{"number": 1, "repo": "someoneelse/private"}))
	if !isErr || !strings.Contains(out, "not a configured repository") {
		t.Fatalf("expected a scope refusal, got isErr=%v out=%q", isErr, out)
	}
	// The refusal should tell the operator what IS in scope.
	if !strings.Contains(out, "Dog279/TENANT") {
		t.Errorf("refusal should list the configured repos: %q", out)
	}
}

// The per-repo authorization: a watch-only repo refuses writes even with the
// most permissive policy possible. This is the property that makes it safe to
// watch a repo you never want touched.
func TestGateWrite_ReadOnlyRepoRefusesEvenWhenApproved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only a write aimed at the READ-ONLY repo is a failure; the writable
		// repo is supposed to reach the network in the second half of this test.
		if r.Method != http.MethodGet && strings.Contains(r.URL.Path, "/bitematch/") {
			t.Errorf("write reached the network for a read-only repo: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	}))
	defer srv.Close()

	d := NewDispatcher(openMulti(t, srv), Policy{
		AllowWrite: true,
		Confirm:    func(context.Context, string, string) bool { return true },
	})

	out, isErr, _ := d.Dispatch(context.Background(), call("github_create_issue",
		map[string]any{"title": "x", "repo": "Dog279/bitematch"}))
	if !isErr || !strings.Contains(out, "watched read-only") {
		t.Fatalf("write to a read-only repo must be refused, got isErr=%v out=%q", isErr, out)
	}

	// ...while the writable repo still works, so the flag is a real distinction
	// and not a blanket block.
	out, isErr, _ = d.Dispatch(context.Background(), call("github_comment",
		map[string]any{"number": 1, "body": "hi", "repo": "Dog279/TENANT"}))
	if isErr && strings.Contains(out, "watched read-only") {
		t.Fatalf("the writable repo must not be refused by the per-repo gate: %q", out)
	}
}

// Reads must work on a watch-only repo - that is the entire point of watching it.
func TestReads_WorkOnWatchOnlyRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/bitematch/issues/3") {
			_, _ = w.Write([]byte(`{"number":3,"title":"Icon assets","state":"open","user":{"login":"Dog279"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := NewDispatcher(openMulti(t, srv), Policy{}) // no write policy at all
	out, isErr, err := d.Dispatch(context.Background(),
		call("github_get_issue", map[string]any{"number": 3, "repo": "Dog279/bitematch"}))
	if err != nil || isErr {
		t.Fatalf("read on a watch-only repo failed: isErr=%v err=%v out=%q", isErr, err, out)
	}
	if !strings.Contains(out, "Icon assets") || !strings.Contains(out, "Dog279/bitematch") {
		t.Errorf("unexpected read output: %q", out)
	}
}

// github_changes fans out across every watched repo when no repo is named, and
// must survive one repo erroring without losing the others.
func TestChanges_FansOutAndSurvivesPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/TENANT/commits"):
			_, _ = w.Write([]byte(`[{"sha":"abcdef1234567","commit":{"message":"fix(route): honor timeout",
				"author":{"name":"Dylan","date":"2026-08-18T09:00:00Z"}}}]`))
		case strings.Contains(r.URL.Path, "/bitematch/commits"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.HasPrefix(r.URL.Path, "/api/v3/search/issues"):
			_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	d := NewDispatcher(openMulti(t, srv), Policy{})
	out, isErr, err := d.Dispatch(context.Background(), call("github_changes", map[string]any{"since": "24h"}))
	if err != nil || isErr {
		t.Fatalf("changes failed: isErr=%v err=%v out=%q", isErr, err, out)
	}
	if !strings.Contains(out, "fix(route): honor timeout") {
		t.Errorf("healthy repo's commit missing from the report:\n%s", out)
	}
	if !strings.Contains(out, "could not read commits") {
		t.Errorf("a failing repo should be reported inline, not swallowed:\n%s", out)
	}
}

func TestParseSince(t *testing.T) {
	now := mustTime(t, "2026-08-18T12:00:00Z")
	cases := map[string]string{
		"":                     "2026-08-17T12:00:00Z",
		"24h":                  "2026-08-17T12:00:00Z",
		"90m":                  "2026-08-18T10:30:00Z",
		"7d":                   "2026-08-11T12:00:00Z",
		"2026-08-01T00:00:00Z": "2026-08-01T00:00:00Z",
	}
	for in, want := range cases {
		got, err := parseSince(in, now)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got.UTC().Format("2006-01-02T15:04:05Z") != want {
			t.Errorf("parseSince(%q) = %s, want %s", in, got.UTC().Format("2006-01-02T15:04:05Z"), want)
		}
	}
	if _, err := parseSince("last tuesday", now); err == nil {
		t.Error("an unparseable window should error rather than silently defaulting")
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return v
}
