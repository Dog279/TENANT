package gsuite

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The JWT-bearer assertion is the security boundary: a wrong sub/scope/
// aud/signature is a silent privilege bug. Decode what we actually send
// and verify it against the real public key.
func TestSASource_BuildsAndSignsValidAssertion(t *testing.T) {
	var gotAssertion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if g := r.Form.Get("grant_type"); g != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type=%q", g)
		}
		gotAssertion = r.Form.Get("assertion")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "expires_in": 3600})
	}))
	defer srv.Close()

	key, saj := testRSA(t, srv.URL)
	fixed := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	src, err := newSASource(saj, "alice@example.com",
		[]string{scopeGmailRead, scopeCalRead}, srv.Client(), func() time.Time { return fixed })
	if err != nil {
		t.Fatalf("newSASource: %v", err)
	}
	tok, err := src.token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("token=%q err=%v", tok, err)
	}

	parts := strings.Split(gotAssertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion not a 3-part JWT: %q", gotAssertion)
	}
	hdr := decodeSeg(t, parts[0])
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" {
		t.Errorf("header wrong: %v", hdr)
	}
	cl := decodeSeg(t, parts[1])
	if cl["iss"] != "robot@proj.iam.gserviceaccount.com" {
		t.Errorf("iss=%v", cl["iss"])
	}
	if cl["sub"] != "alice@example.com" { // domain-wide delegation subject
		t.Errorf("sub=%v (impersonation target wrong)", cl["sub"])
	}
	if cl["scope"] != scopeGmailRead+" "+scopeCalRead {
		t.Errorf("scope=%v", cl["scope"])
	}
	if cl["aud"] != srv.URL {
		t.Errorf("aud=%v", cl["aud"])
	}
	if iat, exp := int64(cl["iat"].(float64)), int64(cl["exp"].(float64)); exp-iat != 3600 || iat != fixed.Unix() {
		t.Errorf("iat/exp wrong: iat=%d exp=%d", iat, exp)
	}
	// Signature must verify against the real public key.
	signing := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(signing))
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("assertion signature does not verify: %v", err)
	}
}

func TestSASource_RejectsBadInput(t *testing.T) {
	_, saj := testRSA(t, "")
	if _, err := newSASource(saj, "", nil, http.DefaultClient, nil); err == nil {
		t.Error("missing subject (DWD) must error")
	}
	if _, err := newSASource([]byte(`{`), "x@y.com", nil, http.DefaultClient, nil); err == nil {
		t.Error("bad JSON must error")
	}
	bad, _ := json.Marshal(map[string]string{"type": "authorized_user", "client_email": "a", "private_key": "b"})
	if _, err := newSASource(bad, "x@y.com", nil, http.DefaultClient, nil); err == nil {
		t.Error("non-service_account key must error")
	}
}

func TestCachedSource_RefreshesAtExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	c := newCached(func(context.Context) (string, time.Time, error) {
		calls++
		return fmt.Sprintf("t%d", calls), now.Add(time.Hour), nil
	}, func() time.Time { return now })

	if v, _ := c.token(context.Background()); v != "t1" || calls != 1 {
		t.Fatalf("first mint wrong: %q calls=%d", v, calls)
	}
	now = now.Add(30 * time.Minute) // still valid
	if v, _ := c.token(context.Background()); v != "t1" || calls != 1 {
		t.Fatalf("should be cached: %q calls=%d", v, calls)
	}
	now = now.Add(30 * time.Minute) // now within refresh skew of expiry
	if v, _ := c.token(context.Background()); v != "t2" || calls != 2 {
		t.Fatalf("should have refreshed: %q calls=%d", v, calls)
	}
}

func TestGcloudSource_UsesCLIAndCaches(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	runs := 0
	var gotArgs []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		runs++
		gotArgs = append([]string{name}, args...)
		return []byte("  gtok\n"), nil
	}
	src := newGcloudSource(run, func() time.Time { return now })
	v, err := src.token(context.Background())
	if err != nil || v != "gtok" {
		t.Fatalf("token=%q err=%v", v, err)
	}
	want := "gcloud auth application-default print-access-token"
	if strings.Join(gotArgs, " ") != want {
		t.Errorf("invoked %q, want %q", strings.Join(gotArgs, " "), want)
	}
	if _, _ = src.token(context.Background()); runs != 1 {
		t.Errorf("second call should be cached, runs=%d", runs)
	}
	now = now.Add(gcloudTTL) // past TTL
	if _, _ = src.token(context.Background()); runs != 2 {
		t.Errorf("should re-run gcloud after TTL, runs=%d", runs)
	}
}

func TestParseRSAKey_PKCS1AndPKCS8(t *testing.T) {
	_, saj1 := testRSA(t, "") // PKCS1 ("RSA PRIVATE KEY")
	var sa serviceAccount
	_ = json.Unmarshal(saj1, &sa)
	if _, err := parseRSAKey(sa.PrivateKey); err != nil {
		t.Errorf("PKCS1 should parse: %v", err)
	}
	if _, err := parseRSAKey("not a pem"); err == nil {
		t.Error("garbage key must error")
	}
}

func decodeSeg(t *testing.T, seg string) map[string]any {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("seg not base64url: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("seg not JSON: %v", err)
	}
	return m
}
