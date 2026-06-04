package x

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// fakeX speaks the X API v2 wire JSON. postBody captures the last
// POST /2/tweets body so write shape can be asserted.
func fakeX(t *testing.T, postBody *map[string]any, deleted *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/2/tweets/search/recent", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "" {
			t.Error("search missing query")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id": "t1", "text": "hello world", "author_id": "u9",
				"created_at":     "2026-05-18T10:00:00Z",
				"public_metrics": map[string]int{"like_count": 5, "retweet_count": 2, "reply_count": 1},
			}},
			"includes": map[string]any{"users": []map[string]string{{"id": "u9", "username": "nasa"}}},
		})
	})
	mux.HandleFunc("/2/tweets", func(w http.ResponseWriter, r *http.Request) { // POST create
		b, _ := io.ReadAll(r.Body)
		if postBody != nil {
			_ = json.Unmarshal(b, postBody)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"id": "new1", "text": "posted"},
		})
	})
	mux.HandleFunc("/2/tweets/", func(w http.ResponseWriter, r *http.Request) { // GET {id} / DELETE {id}
		id := strings.TrimPrefix(r.URL.Path, "/2/tweets/")
		if r.Method == http.MethodDelete {
			if deleted != nil {
				*deleted = id
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]bool{"deleted": true}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"id": id, "text": "a tweet", "author_id": "u9",
				"public_metrics": map[string]int{"like_count": 9}},
			"includes": map[string]any{"users": []map[string]string{{"id": "u9", "username": "nasa"}}},
		})
	})
	mux.HandleFunc("/2/users/by/username/", func(w http.ResponseWriter, r *http.Request) {
		uname := strings.TrimPrefix(r.URL.Path, "/2/users/by/username/")
		if uname == "ghost" {
			_ = json.NewEncoder(w).Encode(map[string]any{}) // no data ⇒ not found
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": "u9", "username": uname, "name": "NASA", "description": "space",
			"verified":       true,
			"public_metrics": map[string]int{"followers_count": 100, "following_count": 2, "tweet_count": 50},
		}})
	})
	mux.HandleFunc("/2/users/", func(w http.ResponseWriter, r *http.Request) { // /2/users/{id}/tweets
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "tt1", "text": "timeline tweet", "author_id": "u9"},
		}})
	})
	return httptest.NewServer(mux)
}

func openFake(t *testing.T, srv *httptest.Server, p Policy, withUser bool) *Dispatcher {
	t.Helper()
	cfg := Config{Bearer: "btok", HTTP: doerTo(t, srv)}
	if withUser {
		cfg.TokenPath = writeStore(t, tokenStore{
			ClientID: "C", Access: "utok", Refresh: "r",
			Expiry: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), // never expires in-test
		})
	}
	svc, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return NewDispatcher(svc, p)
}

func TestReads_SearchTweetUserTimeline(t *testing.T) {
	srv := fakeX(t, nil, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{}, false) // bearer only
	ctx := context.Background()

	out, isErr, _ := d.Dispatch(ctx, call("x_search", map[string]any{"query": "from:nasa"}))
	if isErr || !strings.Contains(out, "@nasa") || !strings.Contains(out, "hello world") || !strings.Contains(out, "♥5") {
		t.Fatalf("search: isErr=%v %q", isErr, out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("x_get_tweet", map[string]any{"id": "t1"}))
	if isErr || !strings.Contains(out, "id=t1") || !strings.Contains(out, "@nasa") {
		t.Fatalf("get_tweet: %q", out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("x_get_user", map[string]any{"username": "@nasa"}))
	if isErr || !strings.Contains(out, "@nasa ✓") || !strings.Contains(out, "followers=100") {
		t.Fatalf("get_user: %q", out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("x_user_timeline", map[string]any{"username": "nasa"}))
	if isErr || !strings.Contains(out, "timeline tweet") {
		t.Fatalf("timeline: %q", out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("x_get_user", map[string]any{"username": "ghost"}))
	if !isErr || !strings.Contains(out, "not found") {
		t.Fatalf("missing user must error: %q", out)
	}
}

func TestPost_ShapeAndReply(t *testing.T) {
	var body map[string]any
	srv := fakeX(t, &body, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowPost: true}, true)
	ctx := context.Background()

	out, isErr, _ := d.Dispatch(ctx, call("x_post", map[string]any{"text": "gm"}))
	if isErr || !strings.Contains(out, "tweet id new1") {
		t.Fatalf("post: isErr=%v %q", isErr, out)
	}
	if body["text"] != "gm" {
		t.Errorf("post body text wrong: %v", body)
	}
	if _, ok := body["reply"]; ok {
		t.Errorf("plain post must not carry reply: %v", body)
	}
	body = nil
	out, isErr, _ = d.Dispatch(ctx, call("x_post", map[string]any{"text": "re", "reply_to": "t1"}))
	if isErr || !strings.Contains(out, "new1") {
		t.Fatalf("reply: %q", out)
	}
	rep, _ := body["reply"].(map[string]any)
	if rep["in_reply_to_tweet_id"] != "t1" {
		t.Errorf("reply target not sent: %v", body)
	}
	// length guard
	long := strings.Repeat("x", 281)
	if out, isErr, _ := d.Dispatch(ctx, call("x_post", map[string]any{"text": long})); !isErr || !strings.Contains(out, "max 280") {
		t.Errorf("over-length must error: %q", out)
	}
}

func TestDelete(t *testing.T) {
	var deleted string
	srv := fakeX(t, nil, &deleted)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowPost: true}, true)
	out, isErr, _ := d.Dispatch(context.Background(), call("x_delete", map[string]any{"id": "t9"}))
	if isErr || !strings.Contains(out, "deleted tweet t9") || deleted != "t9" {
		t.Fatalf("delete: isErr=%v out=%q server-saw=%q", isErr, out, deleted)
	}
}

// Blast-radius boundary: post/delete blocked by default; allowed via
// flag; allowed per-action via Confirm; nothing posted when blocked.
func TestGate_PostBlockedByDefault(t *testing.T) {
	var body map[string]any
	srv := fakeX(t, &body, nil)
	defer srv.Close()
	ctx := context.Background()

	ro := openFake(t, srv, Policy{}, true) // user context present, but no AllowPost
	if out, _, _ := ro.Dispatch(ctx, call("x_search", map[string]any{"query": "x"})); !strings.Contains(out, "hello world") {
		t.Fatalf("read must work: %q", out)
	}
	out, isErr, _ := ro.Dispatch(ctx, call("x_post", map[string]any{"text": "should not send"}))
	if !isErr || !strings.Contains(out, "blocked") {
		t.Fatalf("post must be blocked: isErr=%v %q", isErr, out)
	}
	if body != nil {
		t.Fatalf("post was blocked but a tweet still left: %v", body)
	}
	if out, isErr, _ := ro.Dispatch(ctx, call("x_delete", map[string]any{"id": "t1"})); !isErr || !strings.Contains(out, "blocked") {
		t.Fatalf("delete must be blocked: %q", out)
	}

	var sawDetail string
	cf := openFake(t, srv, Policy{Confirm: func(_ context.Context, _, det string) bool {
		sawDetail = det
		return true
	}}, true)
	if out, isErr, _ := cf.Dispatch(ctx, call("x_post", map[string]any{"text": "ok now"})); isErr || !strings.Contains(out, "new1") {
		t.Fatalf("Confirm should allow the post: isErr=%v %q", isErr, out)
	}
	if !strings.Contains(sawDetail, "ok now") {
		t.Errorf("Confirm detail should describe the post: %q", sawDetail)
	}
}

func TestPost_NeedsUserContext(t *testing.T) {
	srv := fakeX(t, nil, nil)
	defer srv.Close()
	// bearer only (no user token) + AllowPost: gate passes but the API
	// layer must still refuse — a bearer cannot post.
	d := openFake(t, srv, Policy{AllowPost: true}, false)
	out, isErr, _ := d.Dispatch(context.Background(), call("x_post", map[string]any{"text": "hi"}))
	if !isErr || !strings.Contains(out, "user context") {
		t.Fatalf("bearer-only post must refuse with a user-context hint: %q", out)
	}
}

func TestOpen_CredentialSelection(t *testing.T) {
	if _, err := Open(Config{}); err == nil {
		t.Error("no credentials must error")
	}
	svc, err := Open(Config{Bearer: "b"})
	if err != nil || svc.canPost() {
		t.Errorf("bearer-only: err=%v canPost=%v (want false)", err, svc.canPost())
	}
	p := writeStore(t, tokenStore{ClientID: "C", Access: "a", Refresh: "r",
		Expiry: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)})
	svc, err = Open(Config{TokenPath: p})
	if err != nil || !svc.canPost() {
		t.Errorf("token store present: err=%v canPost=%v (want true)", err, svc.canPost())
	}
}

func TestDispatch_BadArgsAndUnknown(t *testing.T) {
	srv := fakeX(t, nil, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowPost: true}, true)
	ctx := context.Background()
	if out, isErr, _ := d.Dispatch(ctx, model.ToolCall{Name: "x_search", Arguments: json.RawMessage(`{bad`)}); !isErr || !strings.Contains(out, "invalid arguments") {
		t.Errorf("bad json: %q", out)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("x_search", map[string]any{"query": " "})); !isErr || !strings.Contains(out, "query is required") {
		t.Errorf("empty query: %q", out)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("nope", nil)); !isErr || !strings.Contains(out, "unknown x tool") {
		t.Errorf("unknown tool: %q", out)
	}
}

func TestTools(t *testing.T) {
	d := NewDispatcher(nil, Policy{})
	names := map[string]bool{}
	for _, sp := range d.Tools() {
		names[sp.Name] = true
		if !json.Valid(sp.Parameters) {
			t.Errorf("%s invalid params schema", sp.Name)
		}
	}
	for _, w := range []string{"x_search", "x_get_tweet", "x_get_user", "x_user_timeline", "x_post", "x_delete"} {
		if !names[w] {
			t.Errorf("missing tool %s", w)
		}
	}
}
