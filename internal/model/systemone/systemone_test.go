package systemone

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The request is the System One shape and each answer type decodes.
func TestDecide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		var req struct {
			Model     string              `json:"model"`
			State     map[string]any      `json:"state"`
			Questions map[string]Question `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil || req.Model != "clef-flash" || req.State["user_request"] != "x" || req.Questions["kind"].Type != Choice {
			t.Errorf("request = %s", b)
		}
		_, _ = w.Write([]byte(`{"model":"clef-flash","answers":{"done":{"type":"noul","noul":0.9},"kind":{"type":"choice","choice":"task","confidence":0.96,"probabilities":{"task":0.96}},"q":{"type":"score","score":1.88,"confidence":0.91}},"usage":{"input_tokens":263,"output_tokens":0}}`))
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL + "/", Model: "clef-flash"}
	qs := map[string]Question{"done": {Type: Noul}, "kind": {Type: Choice, Criteria: map[string]string{"task": "work"}}, "q": {Type: Score, Criteria: []string{"no", "yes"}}}
	a, u, err := c.Decide(context.Background(), map[string]any{"user_request": "x"}, qs)
	if err != nil {
		t.Fatal(err)
	}
	if a["done"].Noul != 0.9 || a["kind"].Choice != "task" || a["q"].Score != 1.88 || u.InputTokens != 263 {
		t.Fatalf("answers %+v usage %+v", a, u)
	}
}

// Server errors keep their message; a missing answer and a timeout are errors.
func TestDecide_Errors(t *testing.T) {
	serve := func(h http.HandlerFunc) *Client {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return &Client{Endpoint: srv.URL, HTTP: &http.Client{Timeout: 100 * time.Millisecond}}
	}
	qs := map[string]Question{"done": {Type: Noul}}
	refused := serve(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error": "at least one question is required"}`))
	})
	if _, _, err := refused.Decide(context.Background(), "s", qs); err == nil || !strings.Contains(err.Error(), "HTTP 400: at least one question is required") {
		t.Errorf("400: err = %v", err)
	}
	missing := serve(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"answers":{}}`)) })
	if _, _, err := missing.Decide(context.Background(), "s", qs); err == nil {
		t.Error("a missing answer should be an error")
	}
	slow := serve(func(w http.ResponseWriter, _ *http.Request) { time.Sleep(400 * time.Millisecond) })
	if _, _, err := slow.Decide(context.Background(), "s", qs); err == nil {
		t.Error("a slow server should time out")
	}
}

// A key goes out as a bearer token; without one the request has no
// Authorization header, which is how a local Clef is called.
func TestDecide_BearerKey(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"answers":{"q":{"type":"noul","noul":0.5}}}`)
	}))
	defer srv.Close()
	qs := map[string]Question{"q": {Type: Noul, Instructions: "?"}}
	if _, _, err := (&Client{Endpoint: srv.URL, Model: "m"}).Decide(context.Background(), "s", qs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&Client{Endpoint: srv.URL, Model: "m", Key: "sek-1"}).Decide(context.Background(), "s", qs); err != nil {
		t.Fatal(err)
	}
	if len(auth) != 2 || auth[0] != "" || auth[1] != "Bearer sek-1" {
		t.Fatalf("Authorization headers = %q", auth)
	}
}
