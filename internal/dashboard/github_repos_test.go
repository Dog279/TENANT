package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeGitHubRepos struct {
	rows      []GitHubRepoRow
	setCalls  []string // "slug:write" / "slug:read"
	addCalls  []string
	rmCalls   []string
	listErr   error
	mutateErr error
}

func (f *fakeGitHubRepos) Repos() ([]GitHubRepoRow, error) { return f.rows, f.listErr }
func (f *fakeGitHubRepos) SetWrite(repo string, write bool) error {
	mode := "read"
	if write {
		mode = "write"
	}
	f.setCalls = append(f.setCalls, repo+":"+mode)
	return f.mutateErr
}
func (f *fakeGitHubRepos) Add(repo string, write bool) error {
	mode := "read"
	if write {
		mode = "write"
	}
	f.addCalls = append(f.addCalls, repo+":"+mode)
	return f.mutateErr
}
func (f *fakeGitHubRepos) Remove(repo string) error {
	f.rmCalls = append(f.rmCalls, repo)
	return f.mutateErr
}

func ghServer(fc GitHubReposControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetGitHubRepos(fc)
	}
	return s
}

func ghPost(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The page must render each watched repo with its current mode preselected -
// otherwise the dropdown lies about the live setting and an operator "confirms"
// a value that was never in effect.
func TestGitHubRepos_PageRendersCurrentMode(t *testing.T) {
	f := &fakeGitHubRepos{rows: []GitHubRepoRow{
		{Slug: "Dog279/TENANT", Write: true, Primary: true},
		{Slug: "Dog279/bitematch", Write: false},
	}}
	s := ghServer(f)

	req := httptest.NewRequest("GET", "/integrations", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, body)
	}

	if !strings.Contains(body, "Dog279/TENANT") || !strings.Contains(body, "Dog279/bitematch") {
		t.Fatalf("watched repos missing from the page:\n%s", body)
	}
	// The writable one offers "Make watch-only" (one click); the watch-only
	// one offers "Allow changes" behind its confirm step.
	if !strings.Contains(body, `<span class="risk">may change it</span>`) || !strings.Contains(body, ">Make watch-only</button>") {
		t.Error("the writable repo should say so and offer Make watch-only")
	}
	if !strings.Contains(body, `<span class="dim">watch-only</span>`) || !strings.Contains(body, `<input type="hidden" name="repo" value="Dog279/bitematch"><input type="hidden" name="mode" value="write"><input type="hidden" name="confirm" value="yes">`) {
		t.Error("the watch-only repo should say so and offer Allow changes through its confirm step")
	}
}

// A repo slug contains a slash, so the target travels as a form value. If it
// were a path param the route would not match at all.
func TestGitHubRepos_ModeFormReachesControl(t *testing.T) {
	f := &fakeGitHubRepos{rows: []GitHubRepoRow{{Slug: "Dog279/bitematch"}}}
	s := ghServer(f)

	if rec := ghPost(t, s, "/integrations/github/repos/mode", "repo=Dog279/bitematch&mode=write&confirm=yes"); rec.Code != http.StatusSeeOther {
		t.Fatalf("mode: status = %d, want 303", rec.Code)
	}
	if len(f.setCalls) != 1 || f.setCalls[0] != "Dog279/bitematch:write" {
		t.Fatalf("SetWrite did not receive the slug intact: %v", f.setCalls)
	}
}

// Anything that is not exactly "write" must be treated as read-only. A typo, a
// truncated body, or a crafted request must never silently grant write.
func TestGitHubRepos_UnknownModeFailsSafe(t *testing.T) {
	for _, mode := range []string{"", "readonly", "WRITE-ISH", "true", "yes"} {
		f := &fakeGitHubRepos{rows: []GitHubRepoRow{{Slug: "a/b"}}}
		s := ghServer(f)
		ghPost(t, s, "/integrations/github/repos/mode", "repo=a/b&confirm=yes&mode="+mode)
		if len(f.setCalls) != 1 {
			t.Fatalf("mode %q: expected one call, got %v", mode, f.setCalls)
		}
		if strings.HasSuffix(f.setCalls[0], ":write") && mode != "WRITE-ISH" {
			t.Errorf("mode %q must not grant write, got %q", mode, f.setCalls[0])
		}
		if f.setCalls[0] != "a/b:read" {
			t.Errorf("mode %q should fail safe to read, got %q", mode, f.setCalls[0])
		}
	}
	// Only the exact value grants write.
	f := &fakeGitHubRepos{rows: []GitHubRepoRow{{Slug: "a/b"}}}
	s := ghServer(f)
	ghPost(t, s, "/integrations/github/repos/mode", "repo=a/b&mode=write&confirm=yes")
	if f.setCalls[0] != "a/b:write" {
		t.Errorf(`exact "write" should grant write, got %q`, f.setCalls[0])
	}
}

func TestGitHubRepos_AddAndRemove(t *testing.T) {
	f := &fakeGitHubRepos{}
	s := ghServer(f)

	if rec := ghPost(t, s, "/integrations/github/repos/add", "repo=Dog279/nebula&mode=write&confirm=yes"); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: status = %d", rec.Code)
	}
	if len(f.addCalls) != 1 || f.addCalls[0] != "Dog279/nebula:write" {
		t.Fatalf("Add did not receive the values: %v", f.addCalls)
	}

	// An empty repo must be rejected before reaching the control.
	f.addCalls = nil
	ghPost(t, s, "/integrations/github/repos/add", "repo=&mode=read")
	if len(f.addCalls) != 0 {
		t.Errorf("an empty repo should not reach the control: %v", f.addCalls)
	}

	if rec := ghPost(t, s, "/integrations/github/repos/remove", "repo=Dog279/nebula"); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: status = %d", rec.Code)
	}
	if len(f.rmCalls) != 1 || f.rmCalls[0] != "Dog279/nebula" {
		t.Fatalf("Remove did not receive the slug: %v", f.rmCalls)
	}
}

// With no control installed the section is hidden and the routes redirect
// rather than panicking on a nil interface.
func TestGitHubRepos_NilControlIsSafe(t *testing.T) {
	s := ghServer(nil)

	req := httptest.NewRequest("GET", "/integrations", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "watched repositories") {
		t.Error("the section should be hidden when no control is installed")
	}
	if rec := ghPost(t, s, "/integrations/github/repos/mode", "repo=a/b&mode=write&confirm=yes"); rec.Code != http.StatusSeeOther {
		t.Errorf("nil control should redirect, got %d", rec.Code)
	}
}
