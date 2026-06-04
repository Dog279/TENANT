package x

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// PKCE is the security boundary: a wrong challenge/method silently
// breaks the flow (or weakens it). Verify the S256 math + auth URL.
func TestPKCE_ChallengeAndAuthURL(t *testing.T) {
	v := newVerifier()
	if len(v) < 43 || len(v) > 128 {
		t.Fatalf("verifier length %d out of RFC 7636 range", len(v))
	}
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := challenge(v); got != want {
		t.Fatalf("challenge = %q, want b64url(sha256(verifier)) = %q", got, want)
	}
	raw := authCodeURL("CID", "http://localhost:8723/cb", scopeRead, "STATE", "CH")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": "CID",
		"redirect_uri": "http://localhost:8723/cb", "scope": scopeRead,
		"state": "STATE", "code_challenge": "CH", "code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			t.Errorf("auth URL %s=%q, want %q", k, q.Get(k), want)
		}
	}
}

func TestPKCESource_RefreshRotatesAndPersists(t *testing.T) {
	var gotForm url.Values
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		gotForm = r.Form
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc-2", "refresh_token": "ref-2", "expires_in": 7200,
		})
	}))
	defer srv.Close()

	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	path := writeStore(t, tokenStore{
		ClientID: "CID", Access: "acc-1", Refresh: "ref-1",
		Expiry: now.Add(-time.Minute), // already expired ⇒ must refresh
	})
	src, err := newPKCESource(path, srv.Client(), func() time.Time { return now })
	if err != nil {
		t.Fatalf("newPKCESource: %v", err)
	}
	src.tok = srv.URL // point token endpoint at the fake

	tok, err := src.token(context.Background())
	if err != nil || tok != "acc-2" {
		t.Fatalf("token=%q err=%v", tok, err)
	}
	if gotForm.Get("grant_type") != "refresh_token" ||
		gotForm.Get("refresh_token") != "ref-1" || gotForm.Get("client_id") != "CID" {
		t.Errorf("refresh form wrong: %v", gotForm)
	}

	// Rotation persisted to disk.
	reloaded, err := loadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Refresh != "ref-2" || reloaded.Access != "acc-2" {
		t.Errorf("rotated token not persisted: %+v", reloaded)
	}
	// Still valid ⇒ cached, no second network call.
	if _, _ = src.token(context.Background()); calls != 1 {
		t.Errorf("expected cached (1 call), got %d", calls)
	}
	// A fresh source reads the rotated token straight from disk.
	src2, err := newPKCESource(path, srv.Client(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	src2.tok = srv.URL
	if v, _ := src2.token(context.Background()); v != "acc-2" {
		t.Errorf("reloaded source token=%q want acc-2", v)
	}
}

func TestExchange_AuthCodeGrant(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "A", "refresh_token": "R", "expires_in": 7200,
		})
	}))
	defer srv.Close()
	tr, err := exchange(context.Background(), srv.Client(), srv.URL, url.Values{
		"grant_type": {"authorization_code"}, "code": {"the-code"},
		"code_verifier": {"the-verifier"}, "client_id": {"CID"},
	})
	if err != nil || tr.AccessToken != "A" || tr.RefreshToken != "R" {
		t.Fatalf("exchange: %+v err=%v", tr, err)
	}
	if form.Get("code") != "the-code" || form.Get("code_verifier") != "the-verifier" {
		t.Errorf("code/verifier not sent: %v", form)
	}
}

func TestExchange_ErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid_grant", "error_description": "expired",
		})
	}))
	defer srv.Close()
	if _, err := exchange(context.Background(), srv.Client(), srv.URL, url.Values{}); err == nil ||
		!strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error should surface invalid_grant, got %v", err)
	}
}

func TestBearerSource(t *testing.T) {
	if _, err := (bearerSource{}).token(context.Background()); err == nil {
		t.Error("empty bearer must error")
	}
	b := bearerSource{"abc"}
	if v, _ := b.token(context.Background()); v != "abc" || b.userContext() {
		t.Errorf("bearer token=%q userContext=%v", v, b.userContext())
	}
}

func TestNewPKCESource_RejectsMissing(t *testing.T) {
	if _, err := newPKCESource("/no/such/file", http.DefaultClient, nil); err == nil {
		t.Error("missing token store must error with a login hint")
	}
	p := writeStore(t, tokenStore{ClientID: "C", Access: "a"}) // no refresh
	if _, err := newPKCESource(p, http.DefaultClient, nil); err == nil {
		t.Error("store without refresh_token must error")
	}
}
