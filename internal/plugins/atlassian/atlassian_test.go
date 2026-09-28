package atlassian

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	common "github.com/ctreminiom/go-atlassian/v2/service/common"
	"tenant/internal/model"
	"tenant/internal/secrets"
)

func TestOpen_PathSelection(t *testing.T) {
	ctx := context.Background()

	// No auth configured → loud error.
	if _, err := Open(ctx, Config{SiteURL: "https://x.atlassian.net"}); err == nil {
		t.Error("no auth path → want error")
	}

	// Path A requires Email.
	if _, err := Open(ctx, Config{SiteURL: "https://x.atlassian.net", APIToken: "t"}); err == nil {
		t.Error("Path A without Email → want error")
	}

	// Path A via explicit token.
	svc, err := Open(ctx, Config{SiteURL: "https://x.atlassian.net", Email: "e@x.com", APIToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Auth != PathAPIToken {
		t.Errorf("auth = %q, want Path A", svc.Auth)
	}

	// Path A via ATLASSIAN_TOKEN env fallback.
	t.Setenv("ATLASSIAN_TOKEN", "envtok")
	svc2, err := Open(ctx, Config{SiteURL: "https://x.atlassian.net", Email: "e@x.com"})
	if err != nil {
		t.Fatalf("env fallback should authenticate: %v", err)
	}
	if svc2.Auth != PathAPIToken {
		t.Error("env token should select Path A")
	}
}

func TestJira_WireContract_PathA(t *testing.T) {
	var authSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/search"):
			// The enhanced endpoint: GET /rest/api/2/search was removed in 2025.
			if r.Method != http.MethodPost || r.URL.Path != "/rest/api/2/search/jql" {
				http.Error(w, `{"errorMessages":["removed endpoint"]}`, http.StatusGone)
				return
			}
			var q struct {
				NextPageToken string `json:"nextPageToken"`
			}
			json.NewDecoder(r.Body).Decode(&q)
			next := "page2"
			if q.NextPageToken == "page2" {
				next = ""
			}
			json.NewEncoder(w).Encode(map[string]any{
				"nextPageToken": next,
				"issues": []map[string]any{{
					"key": "TEN-1",
					"fields": map[string]any{
						"summary":   "first issue",
						"status":    map[string]any{"name": "To Do"},
						"issuetype": map[string]any{"name": "Task"},
					},
				}},
			})
		case strings.Contains(r.URL.Path, "/issue/"):
			json.NewEncoder(w).Encode(map[string]any{
				"key": "TEN-16",
				"fields": map[string]any{
					"summary":     "SoulNudge",
					"status":      map[string]any{"name": "In Progress"},
					"issuetype":   map[string]any{"name": "Task"},
					"description": "the body",
				},
			})
		default:
			w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	svc, err := Open(ctx, Config{SiteURL: srv.URL, Email: "e@x.com", APIToken: "tok", Project: "TEN"})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(svc, Policy{})

	out, isErr, _ := d.Dispatch(ctx, model.ToolCall{Name: "jira_search", Arguments: json.RawMessage(`{"jql":"project=TEN"}`)})
	if isErr || !strings.Contains(out, "TEN-1") || !strings.Contains(out, "first issue") || !strings.Contains(out, `next_page_token "page2"`) {
		t.Fatalf("search: isErr=%v out=%q", isErr, out)
	}
	out, isErr, _ = d.Dispatch(ctx, model.ToolCall{Name: "jira_search", Arguments: json.RawMessage(`{"jql":"project=TEN","next_page_token":"page2"}`)})
	if isErr || !strings.Contains(out, "TEN-1") || strings.Contains(out, "next_page_token") {
		t.Fatalf("last page: isErr=%v out=%q", isErr, out)
	}
	if !strings.HasPrefix(authSeen, "Basic ") {
		t.Errorf("Path A must send Basic auth, got %q", authSeen)
	}

	out, isErr, _ = d.Dispatch(ctx, model.ToolCall{Name: "jira_get", Arguments: json.RawMessage(`{"key":"TEN-16"}`)})
	if isErr || !strings.Contains(out, "SoulNudge") || !strings.Contains(out, "In Progress") {
		t.Fatalf("get: isErr=%v out=%q", isErr, out)
	}
}

func TestGate_WriteBlockedByDefault(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"key":"TEN-99"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	svc, _ := Open(ctx, Config{SiteURL: srv.URL, Email: "e@x.com", APIToken: "t", Project: "TEN"})
	d := NewDispatcher(svc, Policy{}) // AllowWrite=false, no Confirm

	out, isErr, _ := d.Dispatch(ctx, model.ToolCall{Name: "jira_create", Arguments: json.RawMessage(`{"summary":"new"}`)})
	if !isErr || !strings.Contains(out, "blocked") {
		t.Fatalf("write must be blocked by default: isErr=%v out=%q", isErr, out)
	}
	if hit {
		t.Error("gate must block BEFORE any HTTP call to Jira")
	}
}

func TestGate_WriteAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"TEN-99","id":"1001"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	svc, _ := Open(ctx, Config{SiteURL: srv.URL, Email: "e@x.com", APIToken: "t", Project: "TEN"})
	d := NewDispatcher(svc, Policy{AllowWrite: true})

	out, isErr, _ := d.Dispatch(ctx, model.ToolCall{Name: "jira_create", Arguments: json.RawMessage(`{"summary":"new"}`)})
	if isErr || !strings.Contains(out, "TEN-99") {
		t.Fatalf("allowed write should create: isErr=%v out=%q", isErr, out)
	}
}

func TestGate_ConfirmApproves(t *testing.T) {
	p := Policy{Confirm: func(ctx context.Context, action, detail string) bool { return true }}
	if err := p.gate(context.Background(), classWrite, "x"); err != nil {
		t.Errorf("confirm=true should allow the write: %v", err)
	}
	pDeny := Policy{Confirm: func(ctx context.Context, action, detail string) bool { return false }}
	if err := pDeny.gate(context.Background(), classWrite, "x"); err == nil {
		t.Error("confirm=false should block")
	}
	if err := (Policy{}).gate(context.Background(), classRead, "x"); err != nil {
		t.Error("reads are never gated")
	}
}

func TestFileTokenStore_RoundTripAndProtection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "atlassian-token.json")
	s := fileTokenStore{path: path}

	want := &common.OAuth2Token{AccessToken: "acc", RefreshToken: "ref", TokenType: "Bearer", ExpiresIn: 3600}
	if err := s.SetToken(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "acc" || got.RefreshToken != "ref" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if rt, _ := s.GetRefreshToken(ctx); rt != "ref" {
		t.Errorf("GetRefreshToken = %q", rt)
	}
	if report := secrets.Probe(path); !report.Protected {
		t.Errorf("token cache is not protected: %+v", report)
	}
}

// A failed call carries Atlassian's reason: the body's errorMessages and
// errors, or a 429's Retry-After, not go-atlassian's bare "bad request".
func TestJira_ErrorsCarryAtlassianReason(t *testing.T) {
	status := http.StatusBadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"errorMessages":["Unbounded JQL queries are not allowed here."],"errors":{"jql":"add a restriction"}}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	svc, err := Open(ctx, Config{SiteURL: srv.URL, Email: "e@x.com", APIToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(svc, Policy{})

	out, isErr, _ := d.Dispatch(ctx, model.ToolCall{Name: "jira_search", Arguments: json.RawMessage(`{"jql":"ORDER BY created"}`)})
	if !isErr || !strings.Contains(out, "HTTP 400") || !strings.Contains(out, "Unbounded JQL") || !strings.Contains(out, "jql: add a restriction") {
		t.Errorf("400: isErr=%v out=%q", isErr, out)
	}
	status = http.StatusTooManyRequests
	out, isErr, _ = d.Dispatch(ctx, model.ToolCall{Name: "jira_get", Arguments: json.RawMessage(`{"key":"TEN-1"}`)})
	if !isErr || !strings.Contains(out, "rate limited") || !strings.Contains(out, "retry after 30 seconds") {
		t.Errorf("429: isErr=%v out=%q", isErr, out)
	}
}
