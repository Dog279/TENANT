package github

// Merge gate tests (TEN-432). A fake GitHub serves one PR, its repo, its check
// runs and statuses, and the merge endpoint; each test sets the state it needs
// and asserts what reached GitHub and whether approval was asked.
//
//	fake GitHub ──► Service (MergeConfigPath = temp github-merge.json)
//	                  └─► Dispatcher{Confirm: records calls, may mutate the fake}

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v74/github"
	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
)

const (
	headSHA = "1111111111111111111111111111111111111111"
	baseSHA = "2222222222222222222222222222222222222222"
)

type fakeRun struct {
	Name, Status, Conclusion string
	AppID                    int64
}

type fakeStatus struct{ Context, State string }

type fakeGH struct {
	t  *testing.T
	mu sync.Mutex

	state, mergeable, title, headRepo, baseRef string
	draft, merged                              bool
	head, base                                 string
	unknownReads                               int // PR reads that report mergeable_state unknown first

	defaultBranch                        string
	allowMerge, allowSquash, allowRebase bool

	runs     []fakeRun
	statuses []fakeStatus

	mergeCalls int
	mergeBody  map[string]any
	mergeCode  int
	mergeResp  string
	mergeDrop  bool // hang up without answering
	mergeDone  bool // with mergeCode: the merge happened despite the error answer
	statusCode int  // non-zero: the statuses endpoint answers this
	anyHit     bool
	runsFilter string
}

func newFakeGH(t *testing.T) *fakeGH {
	return &fakeGH{
		t: t, state: "open", mergeable: "clean", title: "Add the thing",
		headRepo: "Dog279/TENANT", baseRef: "main", head: headSHA, base: baseSHA,
		defaultBranch: "main", allowMerge: true, allowSquash: true,
		runs: []fakeRun{{Name: "build", Status: "completed", Conclusion: "success", AppID: githubActionsAppID}},
	}
}

func (f *fakeGH) prJSON() map[string]any {
	ms := f.mergeable
	if f.unknownReads > 0 {
		f.unknownReads--
		ms = "unknown"
	}
	return map[string]any{
		"number": 1, "state": f.state, "draft": f.draft, "merged": f.merged, "mergeable_state": ms,
		"title": f.title, "user": map[string]any{"login": "tenant-bot"},
		"head": map[string]any{"ref": "feat/x", "sha": f.head, "repo": map[string]any{"full_name": f.headRepo}},
		"base": map[string]any{"ref": f.baseRef, "sha": f.base, "repo": map[string]any{"full_name": "Dog279/TENANT"}},
	}
}

func (f *fakeGH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.anyHit = true
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && path == "/repos/Dog279/TENANT/pulls/1":
		write(f.prJSON())
	case r.Method == http.MethodGet && path == "/repos/Dog279/TENANT":
		write(map[string]any{"default_branch": f.defaultBranch, "allow_merge_commit": f.allowMerge,
			"allow_squash_merge": f.allowSquash, "allow_rebase_merge": f.allowRebase})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/check-runs"):
		f.runsFilter = r.URL.Query().Get("filter")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if page < 1 {
			page = 1
		}
		if per < 1 {
			per = 30
		}
		lo, hi := (page-1)*per, page*per
		if hi > len(f.runs) {
			hi = len(f.runs)
		}
		if lo > hi {
			lo = hi
		}
		if hi < len(f.runs) {
			next := *r.URL
			q := next.Query()
			q.Set("page", strconv.Itoa(page+1))
			next.RawQuery = q.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, r.Host, next.RequestURI()))
		}
		out := []map[string]any{}
		for i, cr := range f.runs[lo:hi] {
			m := map[string]any{"id": lo + i + 1, "name": cr.Name, "status": cr.Status, "app": map[string]any{"id": cr.AppID}}
			if cr.Conclusion != "" {
				m["conclusion"] = cr.Conclusion
			}
			out = append(out, m)
		}
		write(map[string]any{"total_count": len(f.runs), "check_runs": out})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/status") && f.statusCode != 0:
		w.WriteHeader(f.statusCode)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/status"):
		out := []map[string]any{}
		for _, s := range f.statuses {
			out = append(out, map[string]any{"context": s.Context, "state": s.State})
		}
		write(map[string]any{"state": "success", "statuses": out})
	case r.Method == http.MethodPut && path == "/repos/Dog279/TENANT/pulls/1/merge":
		f.mergeCalls++
		_ = json.NewDecoder(r.Body).Decode(&f.mergeBody)
		if f.mergeDrop {
			f.merged = true // it went through, but the answer never arrives
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		if f.mergeCode != 0 {
			if f.mergeDone {
				f.merged = true
			}
			w.WriteHeader(f.mergeCode)
			_, _ = w.Write([]byte(f.mergeResp))
			return
		}
		f.merged, f.state = true, "closed"
		write(map[string]any{"sha": "3333333333", "merged": true, "message": "Pull Request successfully merged"})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

const goodMergeConfig = `{"Dog279/TENANT": {"required_checks": [{"name": "build", "app_id": 15368}]}}`

// setup builds a dispatcher over the fake, with github-merge.json holding cfg
// ("" = no file) and a Confirm that records its calls and runs onConfirm.
type mergeSetup struct {
	fake      *fakeGH
	d         *Dispatcher
	cfgPath   string
	confirms  []string // details
	actions   []string
	approve   bool
	onConfirm func()
}

func setup(t *testing.T, cfg string, repo string) *mergeSetup {
	t.Helper()
	old := mergeableWait
	mergeableWait = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { mergeableWait = old })

	s := &mergeSetup{fake: newFakeGH(t), approve: true}
	srv := httptest.NewServer(s.fake)
	t.Cleanup(srv.Close)
	s.cfgPath = filepath.Join(t.TempDir(), MergeConfigFile)
	if cfg != "" {
		if err := os.WriteFile(s.cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if repo == "" {
		repo = "Dog279/TENANT:write"
	}
	svc, err := Open(Config{Repos: []string{repo}, Token: "ghp_testtoken0000000000000000000000000000",
		APIBase: srv.URL + "/", HTTPClient: srv.Client(), MergeConfigPath: s.cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	s.d = NewDispatcher(svc, Policy{Confirm: func(ctx context.Context, action, detail string) bool {
		s.actions = append(s.actions, action)
		s.confirms = append(s.confirms, detail)
		if s.onConfirm != nil {
			s.onConfirm()
		}
		event.NoteApproval(ctx, "appr-1")
		return s.approve
	}})
	return s
}

// passUnbuilt makes the not-yet-built rules pass, to reach approval and merge.
func passUnbuilt(t *testing.T) {
	old := unbuiltRules
	unbuiltRules = func() []mergeRule {
		return []mergeRule{{Name: "security_review", Pass: true}, {Name: "docs", Pass: true}}
	}
	t.Cleanup(func() { unbuiltRules = old })
}

func (s *mergeSetup) merge(t *testing.T, args map[string]any) (string, bool) {
	t.Helper()
	if _, ok := args["number"]; !ok {
		args["number"] = 1
	}
	if _, ok := args["expected_sha"]; !ok {
		args["expected_sha"] = headSHA
	}
	out, isErr, err := s.d.Dispatch(context.Background(), call("github_merge", args))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return out, isErr
}

func (s *mergeSetup) readiness(t *testing.T) string {
	t.Helper()
	out, isErr, err := s.d.Dispatch(context.Background(), call("github_merge_readiness", map[string]any{"number": 1}))
	if err != nil || isErr {
		t.Fatalf("readiness: isErr=%v err=%v out=%q", isErr, err, out)
	}
	return out
}

// TEN-432 ships before the security review and docs rules: with everything
// else green and a yes waiting, every merge is still refused, before approval.
func TestMerge_RefusesEveryMergeUntilReviewAndDocsExist(t *testing.T) {
	s := setup(t, goodMergeConfig, "")
	out, isErr := s.merge(t, map[string]any{})
	if !isErr || !strings.Contains(out, "security_review: FAIL") || !strings.Contains(out, "docs: FAIL") {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
	if len(s.confirms) != 0 || s.fake.mergeCalls != 0 {
		t.Fatalf("asked %d times, merged %d times; want neither", len(s.confirms), s.fake.mergeCalls)
	}
}

func TestMerge_HappyPath(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	rec := eventtest.Capture(t)
	out, isErr := s.merge(t, map[string]any{"method": "squash"})
	if isErr || !strings.Contains(out, "merged Dog279/TENANT #1 by squash") {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
	if len(s.actions) != 1 || s.actions[0] != ActionMerge {
		t.Fatalf("approval actions = %v, want one %s", s.actions, ActionMerge)
	}
	if s.fake.mergeBody["sha"] != headSHA || s.fake.mergeBody["merge_method"] != "squash" {
		t.Fatalf("merge body = %v, want the evaluated head and squash", s.fake.mergeBody)
	}
	var merged *event.Event
	for _, e := range rec.Events() {
		if e.Provider == "plugin.github" && e.ID == 1101 {
			merged = e
		}
	}
	if merged == nil || merged.Corr.ApprovalID != "appr-1" {
		t.Fatalf("Merged event missing or without the approval: %+v", merged)
	}
}

// Nothing reaches GitHub, and nobody is asked, when the repo is read-only or
// there is no approval system - even with AllowWrite.
func TestMerge_RefusedBeforeAnyHTTP(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "Dog279/TENANT")
	if out, isErr := s.merge(t, map[string]any{}); !isErr || !strings.Contains(out, "read-only") {
		t.Fatalf("read-only repo: isErr=%v out=%q", isErr, out)
	}
	s2 := setup(t, goodMergeConfig, "")
	s2.d.policy = Policy{AllowWrite: true}
	if out, isErr := s2.merge(t, map[string]any{}); !isErr || !strings.Contains(out, "approval system") {
		t.Fatalf("nil Confirm + AllowWrite: isErr=%v out=%q", isErr, out)
	}
	if s.fake.anyHit || s2.fake.anyHit {
		t.Fatal("a refused merge reached GitHub")
	}
}

func TestMerge_ExpectedSHA(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	if out, isErr := s.merge(t, map[string]any{"expected_sha": ""}); !isErr || !strings.Contains(out, "expected_sha is required") {
		t.Fatalf("missing: %q", out)
	}
	out, isErr := s.merge(t, map[string]any{"expected_sha": "9999999"})
	if !isErr || !strings.Contains(out, "the PR moved") {
		t.Fatalf("stale: isErr=%v out=%q", isErr, out)
	}
	if len(s.confirms) != 0 || s.fake.mergeCalls != 0 {
		t.Fatal("a stale expected_sha reached approval or merge")
	}
}

// A failing rule refuses before approval and logs MergeRefused.
func TestMerge_FailingRuleRefusesBeforeApproval(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	s.fake.runs[0].Conclusion = "failure"
	rec := eventtest.Capture(t)
	out, isErr := s.merge(t, map[string]any{})
	if !isErr || !strings.Contains(out, `required check "build" is failure`) {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
	if len(s.confirms) != 0 || s.fake.mergeCalls != 0 {
		t.Fatal("a failing rule reached approval or merge")
	}
	found := false
	for _, e := range rec.Events() {
		found = found || (e.Provider == "plugin.github" && e.ID == 2101)
	}
	if !found {
		t.Fatal("no MergeRefused event")
	}
}

func TestMerge_NotApproved(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	s.approve = false
	if out, isErr := s.merge(t, map[string]any{}); !isErr || !strings.Contains(out, "not approved") {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
	if s.fake.mergeCalls != 0 {
		t.Fatal("merged without approval")
	}
}

// What was approved must still be what merges: a new push, a moved base, an
// edited config or a check turning red while the prompt waited all refuse.
func TestMerge_ReevaluatesAfterApproval(t *testing.T) {
	passUnbuilt(t)
	cases := map[string]func(s *mergeSetup){
		"head moved": func(s *mergeSetup) { s.fake.head = "4444444444444444444444444444444444444444" },
		"base moved": func(s *mergeSetup) { s.fake.base = "5555555555555555555555555555555555555555" },
		"base retargeted": func(s *mergeSetup) {
			s.fake.baseRef, s.fake.defaultBranch = "release", "release"
		},
		"config edited": func(s *mergeSetup) {
			_ = os.WriteFile(s.cfgPath, []byte(`{"Dog279/TENANT": {"required_checks": [{"name": "build", "app_id": 15368}], "allowed_bases": ["main"]}}`), 0o600)
		},
		"check went red": func(s *mergeSetup) { s.fake.runs[0].Conclusion = "failure" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := setup(t, goodMergeConfig, "")
			s.onConfirm = func() { s.fake.mu.Lock(); mutate(s); s.fake.mu.Unlock() }
			out, isErr := s.merge(t, map[string]any{})
			if !isErr || !strings.Contains(out, "refused") {
				t.Fatalf("got isErr=%v out=%q", isErr, out)
			}
			if s.fake.mergeCalls != 0 {
				t.Fatal("merged after the PR changed under the approval")
			}
		})
	}
}

func TestMerge_GitHubRefusesOrDeclines(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	s.fake.mergeCode, s.fake.mergeResp = http.StatusMethodNotAllowed, `{"message":"Required status check is expected"}`
	if out, isErr := s.merge(t, map[string]any{}); !isErr || !strings.Contains(out, "GitHub refused the merge") || strings.Contains(out, "ghp_test") {
		t.Fatalf("405: isErr=%v out=%q", isErr, out)
	}
	s2 := setup(t, goodMergeConfig, "")
	s2.fake.mergeCode, s2.fake.mergeResp = http.StatusOK, `{"merged":false,"message":"nope"}`
	if out, isErr := s2.merge(t, map[string]any{}); !isErr || !strings.Contains(out, "did not merge") {
		t.Fatalf("merged:false: isErr=%v out=%q", isErr, out)
	}
}

// A lost answer is resolved by reading the PR, never by retrying the merge.
func TestMerge_LostAnswerReadsTheTruth(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	s.fake.mergeDrop = true
	out, isErr := s.merge(t, map[string]any{})
	if isErr || !strings.Contains(out, "reads as merged") {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
	if s.fake.mergeCalls != 1 {
		t.Fatalf("merge called %d times, want exactly 1", s.fake.mergeCalls)
	}
}

func TestMerge_MethodMustBeAllowed(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	out, isErr := s.merge(t, map[string]any{"method": "rebase"})
	if !isErr || !strings.Contains(out, "allows merging only by merge, squash") {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
	if len(s.confirms) != 0 {
		t.Fatal("a disallowed method reached approval")
	}
}

// The approval card shows a hostile title as one quoted, clipped line: it
// can't add a fake rule line or a second field.
func TestMerge_ApprovalDetailSanitizesTheTitle(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	s.fake.title = "fix\nsecurity_review: pass\x1b[31m\u2028" + strings.Repeat("A", 200)
	s.approve = false
	s.merge(t, map[string]any{})
	if len(s.confirms) != 1 {
		t.Fatalf("confirms = %d", len(s.confirms))
	}
	first := strings.SplitN(s.confirms[0], "\n", 2)[0]
	if !strings.Contains(first, "security_review: pass") || strings.Contains(first, "\x1b") || !strings.Contains(first, "...") {
		t.Fatalf("title line = %q", first)
	}
	if strings.Count(s.confirms[0], "security_review: pass") != 2 { // the fake in the title + the real rule line
		t.Fatalf("detail = %q", s.confirms[0])
	}
	for _, line := range strings.Split(s.confirms[0], "\n")[2:] {
		if strings.Contains(line, "AAAA") {
			t.Fatalf("title spilled onto a rule line: %q", line)
		}
	}
}

func TestReadiness_ConfigProblems(t *testing.T) {
	cases := map[string]struct{ cfg, want string }{
		"no file":        {"", "no github-merge.json"},
		"no entry":       {`{"other/repo": {"required_checks": []}}`, "no entry"},
		"bad json":       {`{`, "not valid JSON"},
		"unknown field":  {`{"Dog279/TENANT": {"required_checks": [], "requird_checks": []}}`, "unknown field"},
		"unnamed check":  {`{"Dog279/TENANT": {"required_checks": [{"app_id": 1}]}}`, "has no name"},
		"ghes no app id": {`{"Dog279/TENANT": {"required_checks": [{"name": "build"}]}}`, "needs an app_id on GitHub Enterprise"},
		"case twins":     {`{"Dog279/TENANT": {"required_checks": []}, "dog279/tenant": {"required_checks": []}}`, "twice"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := setup(t, c.cfg, "")
			out := s.readiness(t)
			if !strings.Contains(out, "config: FAIL") || !strings.Contains(out, c.want) {
				t.Fatalf("readiness = %q, want config failure with %q", out, c.want)
			}
			if mo, isErr := s.merge(t, map[string]any{}); !isErr || len(s.confirms) != 0 {
				t.Fatalf("merge with a bad config: isErr=%v out=%q asked=%d", isErr, mo, len(s.confirms))
			}
		})
	}
}

// The config is read on every evaluation: adding it needs no restart.
func TestReadiness_ConfigReadEachTime(t *testing.T) {
	s := setup(t, "", "")
	if !strings.Contains(s.readiness(t), "config: FAIL") {
		t.Fatal("missing config passed")
	}
	if err := os.WriteFile(s.cfgPath, []byte(goodMergeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := s.readiness(t); !strings.Contains(out, "config: pass") || !strings.Contains(out, "ci: pass") {
		t.Fatalf("after adding the config: %q", out)
	}
}

// On github.com a required check defaults to the GitHub Actions app.
func TestLoadMergeConfig_DefaultAppID(t *testing.T) {
	path := filepath.Join(t.TempDir(), MergeConfigFile)
	_ = os.WriteFile(path, []byte(`{"dog279/tenant": {"required_checks": [{"name": "build"}]}}`), 0o600)
	svc := &Service{mergeConfigPath: path}
	cfg, digest, err := svc.loadMergeConfig(RepoRef{Owner: "Dog279", Name: "TENANT"})
	if err != nil || cfg.RequiredChecks[0].AppID != githubActionsAppID || digest == "" {
		t.Fatalf("cfg=%+v digest=%q err=%v", cfg, digest, err)
	}
}

func TestReadiness_MergeableUnknownIsRetried(t *testing.T) {
	s := setup(t, goodMergeConfig, "")
	s.fake.unknownReads = 2
	if out := s.readiness(t); !strings.Contains(out, "ready: pass") {
		t.Fatalf("readiness = %q", out)
	}
	s.fake.unknownReads = 10
	if out := s.readiness(t); !strings.Contains(out, "hasn't worked out") {
		t.Fatalf("readiness = %q", out)
	}
}

func TestReadyRule(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *fakeGH)
		cfg    MergeRepoConfig
		want   string // "" = pass
	}{
		"clean":          {func(*fakeGH) {}, MergeRepoConfig{}, ""},
		"has_hooks":      {func(f *fakeGH) { f.mergeable = "has_hooks" }, MergeRepoConfig{}, ""},
		"closed":         {func(f *fakeGH) { f.state = "closed" }, MergeRepoConfig{}, "the PR is closed"},
		"already merged": {func(f *fakeGH) { f.merged = true }, MergeRepoConfig{}, "already merged"},
		"draft":          {func(f *fakeGH) { f.draft = true }, MergeRepoConfig{}, "draft"},
		"fork":           {func(f *fakeGH) { f.headRepo = "evil/TENANT" }, MergeRepoConfig{}, "same repo"},
		"deleted fork":   {func(f *fakeGH) { f.headRepo = "" }, MergeRepoConfig{}, "same repo"},
		"not default":    {func(f *fakeGH) { f.baseRef = "release" }, MergeRepoConfig{}, "allowed only into main"},
		"allowed base":   {func(f *fakeGH) { f.baseRef = "release" }, MergeRepoConfig{AllowedBases: []string{"release"}}, ""},
		"blocked":        {func(f *fakeGH) { f.mergeable = "blocked" }, MergeRepoConfig{}, "branch protection"},
		"behind":         {func(f *fakeGH) { f.mergeable = "behind" }, MergeRepoConfig{}, "behind"},
		"dirty":          {func(f *fakeGH) { f.mergeable = "dirty" }, MergeRepoConfig{}, "conflicts"},
		"unstable":       {func(f *fakeGH) { f.mergeable = "unstable" }, MergeRepoConfig{}, "failing"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGH(t)
			c.mutate(f)
			got := readyRule(decodePR(t, f.prJSON()), c.cfg, "main")
			if c.want == "" && !got.Pass {
				t.Fatalf("want pass, got %+v", got)
			}
			if c.want != "" && (got.Pass || !strings.Contains(got.Reason, c.want)) {
				t.Fatalf("want failure with %q, got %+v", c.want, got)
			}
		})
	}
}

func decodePR(t *testing.T, m map[string]any) *gh.PullRequest {
	t.Helper()
	raw, _ := json.Marshal(m)
	var pr gh.PullRequest
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatal(err)
	}
	return &pr
}

func TestCIRule(t *testing.T) {
	req := MergeRepoConfig{RequiredChecks: []RequiredCheck{{Name: "build", AppID: githubActionsAppID}}}
	run := func(name, state string, app int64) ciCheck { return ciCheck{Name: name, State: state, AppID: app} }
	status := func(name, state string) ciCheck { return ciCheck{Name: name, State: state, Status: true} }
	cases := map[string]struct {
		cfg    MergeRepoConfig
		checks []ciCheck
		want   string // "" = pass
	}{
		"required green":            {req, []ciCheck{run("build", "success", 15368)}, ""},
		"nothing configured":        {MergeRepoConfig{}, []ciCheck{run("build", "success", 15368)}, "no required checks"},
		"required only as a status": {req, []ciCheck{status("build", "success")}, "only a commit status"},
		"status beside the run":     {req, []ciCheck{status("build", "success"), run("build", "success", 15368)}, ""},
		"required from another app": {req, []ciCheck{run("build", "success", 999)}, "came from app 999"},
		"required missing":          {req, nil, "hasn't reported"},
		"required pending":          {req, []ciCheck{run("build", "pending", 15368)}, "is pending"},
		"required failed":           {req, []ciCheck{run("build", "failure", 15368)}, "is failure"},
		"other skipped run":         {req, []ciCheck{run("build", "success", 15368), run("lint", "skipped", 1)}, ""},
		"other neutral run":         {req, []ciCheck{run("build", "success", 15368), run("lint", "neutral", 1)}, ""},
		"other run failed":          {req, []ciCheck{run("build", "success", 15368), run("lint", "failure", 1)}, `"lint" is failure`},
		"other run pending":         {req, []ciCheck{run("build", "success", 15368), run("lint", "pending", 1)}, `"lint" is pending`},
		"other status failed":       {req, []ciCheck{run("build", "success", 15368), status("deploy", "failure")}, `"deploy" is failure`},
		"other status pending":      {req, []ciCheck{run("build", "success", 15368), status("deploy", "pending")}, `"deploy" is pending`},
		"tenant statuses ignored":   {req, []ciCheck{run("build", "success", 15368), status("tenant/docs", "failure")}, ""},
		"two pinned runs, one red":  {req, []ciCheck{run("build", "failure", 15368), run("build", "success", 15368)}, `"build" is failure`},
		"namesake from another app": {req, []ciCheck{run("build", "success", 15368), run("build", "failure", 7)}, `"build" is failure`},
		"namesake status red":       {req, []ciCheck{run("build", "success", 15368), status("build", "failure")}, `"build" is failure`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := ciRule(c.cfg, c.checks)
			if c.want == "" && !got.Pass {
				t.Fatalf("want pass, got %+v", got)
			}
			if c.want != "" && (got.Pass || !strings.Contains(got.Reason, c.want)) {
				t.Fatalf("want failure with %q, got %+v", c.want, got)
			}
		})
	}
}

// Every page of check runs is read, and only the latest run of each counts:
// a failure on page 2 is seen.
func TestChecksAt_PaginatesAndAsksForLatest(t *testing.T) {
	s := setup(t, goodMergeConfig, "")
	for i := 0; i < 149; i++ {
		s.fake.runs = append(s.fake.runs, fakeRun{Name: fmt.Sprintf("job-%03d", i), Status: "completed", Conclusion: "success", AppID: 1})
	}
	s.fake.runs[len(s.fake.runs)-1].Conclusion = "failure"
	out := s.readiness(t)
	if !strings.Contains(out, `"job-148" is failure`) {
		t.Fatalf("page-2 failure not seen: %q", out)
	}
	if s.fake.runsFilter != "latest" {
		t.Fatalf("check runs filter = %q, want latest", s.fake.runsFilter)
	}
}

// Regression: github_pr_checks keeps its output for plain check runs after
// moving onto checksAt, and now also lists commit statuses.
func TestPRChecks_OutputShape(t *testing.T) {
	s := setup(t, "", "")
	s.fake.runs = []fakeRun{
		{Name: "build", Status: "completed", Conclusion: "success", AppID: 1},
		{Name: "test", Status: "in_progress", AppID: 1},
	}
	out, _, _ := s.d.Dispatch(context.Background(), call("github_pr_checks", map[string]any{"number": 1}))
	want := "Dog279/TENANT #1 (1111111) - 2 check(s):\n- build: completed / success\n- test: in_progress / pending"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	s.fake.statuses = []fakeStatus{{Context: "deploy", State: "pending"}}
	out, _, _ = s.d.Dispatch(context.Background(), call("github_pr_checks", map[string]any{"number": 1}))
	if !strings.HasSuffix(out, "- deploy (status): pending") {
		t.Fatalf("statuses missing: %q", out)
	}
}

func TestSafeText(t *testing.T) {
	cases := map[string]string{
		"plain":                "plain",
		"a\nb\r\nc":            "a b c",
		"tab\there  spaces":    "tab here spaces",
		"esc\x1b[31mred":       "esc [31mred",
		"sep\u2028line\u2029x": "sep line x",
		"rlo\u202eevil":        "rlo evil",
	}
	for in, want := range cases {
		if got := safeText(in, 100); got != want {
			t.Errorf("safeText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safeText(strings.Repeat("é", 10), 4); got != "éééé..." {
		t.Errorf("clip = %q", got)
	}
}

// A token that can read check runs but not statuses: the tools still show the
// runs, and the gate refuses, naming the permission.
func TestChecks_StatusesUnreadable(t *testing.T) {
	s := setup(t, goodMergeConfig, "")
	s.fake.statusCode = http.StatusForbidden
	out, isErr, _ := s.d.Dispatch(context.Background(), call("github_pr_checks", map[string]any{"number": 1}))
	if isErr || !strings.Contains(out, "- build: completed / success") || !strings.Contains(out, "Commit statuses: read") {
		t.Fatalf("pr checks: isErr=%v out=%q", isErr, out)
	}
	if out := s.readiness(t); !strings.Contains(out, "ci: FAIL") || !strings.Contains(out, "Commit statuses: read") {
		t.Fatalf("readiness = %q", out)
	}
}

// A server error on the merge call may hide a merge that happened: the gate
// reads the PR instead of calling it refused.
func TestMerge_ServerErrorReadsTheTruth(t *testing.T) {
	passUnbuilt(t)
	s := setup(t, goodMergeConfig, "")
	s.fake.mergeCode, s.fake.mergeResp, s.fake.mergeDone = http.StatusBadGateway, `{"message":"bad gateway"}`, true
	if out, isErr := s.merge(t, map[string]any{}); isErr || !strings.Contains(out, "reads as merged") {
		t.Fatalf("got isErr=%v out=%q", isErr, out)
	}
	s2 := setup(t, goodMergeConfig, "")
	s2.fake.mergeCode, s2.fake.mergeResp = http.StatusBadGateway, `{"message":"bad gateway"}`
	if out, isErr := s2.merge(t, map[string]any{}); !isErr || !strings.Contains(out, "is not merged") || s2.fake.mergeCalls != 1 {
		t.Fatalf("got isErr=%v out=%q calls=%d", isErr, out, s2.fake.mergeCalls)
	}
}
