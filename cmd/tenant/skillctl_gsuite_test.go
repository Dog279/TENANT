package main

// TEN-65: gsuite catalog entry tests — validators, probe (both auth
// branches via httptest), default application, doctor integration.
//
// No live Google calls. Every Google endpoint is httptest-backed,
// mirroring the pattern in internal/plugins/gsuite/api_test.go.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// --- helpers (mirror gsuite/helpers_test.go's rewriteDoer + testRSA) ---

type gsuiteRewriteDoer struct {
	base *url.URL
	c    *http.Client
}

func (d gsuiteRewriteDoer) Do(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = d.base.Scheme
	r.URL.Host = d.base.Host
	r.Host = d.base.Host
	return d.c.Do(r)
}

func gsuiteDoerTo(t *testing.T, srv *httptest.Server) gsuiteRewriteDoer {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return gsuiteRewriteDoer{base: u, c: srv.Client()}
}

// fakeSAClientID is the numeric OAuth client ID embedded in test SA keys —
// the value the DWD remediation hint must echo back to the operator.
const fakeSAClientID = "100000000000000000001"

// writeFakeSAJSON drops a service-account-shaped JSON file into a
// temp dir and returns its path. Uses a real generated RSA key so
// the gsuite SA path can mint/sign a JWT against it.
func writeFakeSAJSON(t *testing.T, tokenURI string) (path, clientEmail string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	clientEmail = "robot@proj.iam.gserviceaccount.com"
	saj, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": clientEmail,
		"client_id":    fakeSAClientID,
		"private_key":  string(pemBytes),
		"token_uri":    tokenURI,
	})
	path = filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, saj, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, clientEmail
}

// --- validators ----------------------------------------------------

func TestValidateSAJSON_EmptyOK(t *testing.T) {
	// sa_json is optional at the framework level; probe enforces in sa mode.
	if err := validateSAJSON(""); err != nil {
		t.Errorf("empty path should pass; got %v", err)
	}
}

func TestValidateSAJSON_FileMissing(t *testing.T) {
	err := validateSAJSON(filepath.Join(t.TempDir(), "nosuch.json"))
	if err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Errorf("missing file should fail with 'file not found'; got %v", err)
	}
}

func TestValidateSAJSON_NotJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(p, []byte("not json {"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := validateSAJSON(p)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("malformed JSON should fail; got %v", err)
	}
}

func TestValidateSAJSON_JSONButWrongType(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wrong.json")
	if err := os.WriteFile(p, []byte(`{"type":"user_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := validateSAJSON(p)
	if err == nil || !strings.Contains(err.Error(), "not a service-account key") {
		t.Errorf("wrong type should fail; got %v", err)
	}
}

func TestValidateSAJSON_Directory(t *testing.T) {
	err := validateSAJSON(t.TempDir()) // a dir, not a file
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("directory path should fail; got %v", err)
	}
}

func TestValidateSAJSON_ValidServiceAccount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "good.json")
	body := `{"type":"service_account","client_email":"x@y","private_key":"pem"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateSAJSON(p); err != nil {
		t.Errorf("valid SA JSON should pass; got %v", err)
	}
}

func TestValidateEmailRFC5322(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"", true}, // optional
		{"user@example.com", true},
		{"first.last+tag@sub.example.com", true},
		{"not-an-email", false},
		{"missing@", false},
		{"@missing.local", false},
		{"two@@signs.com", false},
	}
	for _, c := range cases {
		err := validateEmailRFC5322(c.in)
		if (err == nil) != c.ok {
			t.Errorf("validateEmailRFC5322(%q): wanted ok=%v; got err=%v", c.in, c.ok, err)
		}
	}
}

// --- catalog entry sanity -----------------------------------------

func TestGSuiteCatalogEntry_Shape(t *testing.T) {
	k, ok := skillKinds["gsuite"]
	if !ok {
		t.Fatal("gsuite missing from skillKinds")
	}
	if !k.Wired {
		t.Error("gsuite should be Wired")
	}
	if k.Probe == nil {
		t.Error("gsuite should have a Probe")
	}
	keys := map[string]bool{}
	for _, f := range k.Fields {
		keys[f.Key] = true
	}
	for _, want := range []string{"auth", "sa_json", "subject"} {
		if !keys[want] {
			t.Errorf("gsuite missing field %q (have: %v)", want, keys)
		}
	}
	// auth must have a Default in the catalog's Options set, and must
	// be Required. (Catalog default was bumped from "gcloud" to "oauth"
	// in the handhold-UX rework — oauth is the easiest path for non-
	// developers and matches the recommended order in the picker.)
	for _, f := range k.Fields {
		if f.Key == "auth" {
			validDefaults := map[string]bool{"composio": true, "oauth": true, "gcloud": true, "sa": true}
			if !validDefaults[f.Default] {
				t.Errorf("auth Default must be one of {composio,oauth,gcloud,sa}; got %q", f.Default)
			}
			if !f.Required {
				t.Error("auth should be Required")
			}
			if f.Default == "" {
				t.Error("auth should have a Default")
			}
		}
	}
}

// --- probeGSuite — gcloud branch -----------------------------------

// fakeGmailServer returns a stub httptest server that responds to
// the Gmail list endpoint with an EMPTY result set. Empty proves the
// token authenticated (200 status) without forcing Search to fan out
// to per-message metadata fetches (which would require more mock
// handlers).
func fakeGmailServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[],"resultSizeEstimate":0}`))
	})
	return httptest.NewServer(mux)
}

func TestProbeGSuite_GcloudSuccess(t *testing.T) {
	srv := fakeGmailServer(t)
	defer srv.Close()
	deps := gsuiteProbeDeps{
		HTTP: gsuiteDoerTo(t, srv),
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("fake-token"), nil
		},
		GcloudCheck: func() error { return nil }, // pretend gcloud is on PATH
	}
	identity, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{"auth": "gcloud"}, func() error { return nil }, deps)
	if err != nil {
		t.Fatalf("probe should succeed; got %v", err)
	}
	if !strings.Contains(identity, "gcloud ADC authenticated") {
		t.Errorf("identity wrong; got %q", identity)
	}
}

func TestProbeGSuite_GcloudNotOnPath(t *testing.T) {
	deps := gsuiteProbeDeps{
		GcloudCheck: func() error {
			return errors.New("gcloud not found on PATH — install Cloud SDK, OR switch to auth=sa")
		},
	}
	_, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{"auth": "gcloud"}, func() error { return nil }, deps)
	if err == nil || !strings.Contains(err.Error(), "gcloud not found") {
		t.Errorf("missing gcloud should fail clean; got %v", err)
	}
}

func TestProbeGSuite_GcloudGmailFails(t *testing.T) {
	// Server returns 401 on Gmail call — token mints but call fails.
	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid_grant","status":"UNAUTHENTICATED"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	deps := gsuiteProbeDeps{
		HTTP: gsuiteDoerTo(t, srv),
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("bad-token"), nil
		},
		GcloudCheck: func() error { return nil },
	}
	_, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{"auth": "gcloud"}, func() error { return nil }, deps)
	if err == nil || !strings.Contains(err.Error(), "Gmail probe failed") {
		t.Errorf("401 should surface as Gmail probe failed; got %v", err)
	}
}

// --- probeGSuite — sa branch ---------------------------------------

func TestProbeGSuite_SASuccess(t *testing.T) {
	// Two endpoints: token exchange + Gmail list.
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sa-token","expires_in":3600}`))
	})
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[],"resultSizeEstimate":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Write SA JSON pointing at the test token endpoint.
	saPath, _ := writeFakeSAJSON(t, srv.URL+"/token")

	deps := gsuiteProbeDeps{HTTP: gsuiteDoerTo(t, srv)}
	identity, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{
		"auth":    "sa",
		"sa_json": saPath,
		"subject": "alice@example.com",
	}, func() error { return nil }, deps)
	if err != nil {
		t.Fatalf("SA probe should succeed; got %v", err)
	}
	if !strings.Contains(identity, "alice@example.com") || !strings.Contains(identity, "impersonated") {
		t.Errorf("SA identity should name subject + 'impersonated'; got %q", identity)
	}
}

// A 401 unauthorized_client from the token endpoint — the classic "DWD was
// authorized for a DIFFERENT service account / scope set" trap — must yield
// an ACTIONABLE hint (the numeric client ID to authorize, the exact scopes,
// and the Admin-console path), not just Google's cryptic 401. This is the
// real failure user@example.com hit reusing a prior OpenClaw SA.
func TestProbeGSuite_SA_UnauthorizedClientHint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens using this method, or client not authorized for any of the scopes requested."}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	saPath, _ := writeFakeSAJSON(t, srv.URL+"/token")
	deps := gsuiteProbeDeps{HTTP: gsuiteDoerTo(t, srv)}
	_, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{
		"auth":    "sa",
		"sa_json": saPath,
		"subject": "alice@example.com",
	}, func() error { return nil }, deps)
	if err == nil {
		t.Fatal("unauthorized_client must surface as an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"Gmail probe failed",     // prefix the catalog/tests key on
		"Domain-Wide Delegation", // names the actual subsystem
		fakeSAClientID,           // the exact numeric client ID to authorize
		"gmail.readonly",         // read scopes the probe needs
		"gmail.send",             // write scopes for full functionality
		"admin.google.com",       // where to fix it
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("DWD hint missing %q; got:\n%s", want, msg)
		}
	}
}

func TestProbeGSuite_SAMissingSubject(t *testing.T) {
	saPath, _ := writeFakeSAJSON(t, "https://example.com/token")
	_, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{"auth": "sa", "sa_json": saPath /* no subject */},
		func() error { return nil }, gsuiteProbeDeps{})
	if err == nil || !strings.Contains(err.Error(), "subject") {
		t.Errorf("SA without subject should fail naming subject; got %v", err)
	}
}

func TestProbeGSuite_SAMissingSAJSON(t *testing.T) {
	_, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{"auth": "sa", "subject": "x@y.com" /* no sa_json */},
		func() error { return nil }, gsuiteProbeDeps{})
	if err == nil || !strings.Contains(err.Error(), "sa_json") {
		t.Errorf("SA without sa_json should fail naming sa_json; got %v", err)
	}
}

func TestProbeGSuite_UnknownAuth(t *testing.T) {
	_, err := probeGSuiteWith(context.Background(), &credentials{Secrets: map[string]string{}}, map[string]string{"auth": "nope"}, func() error { return nil }, gsuiteProbeDeps{})
	if err == nil || !strings.Contains(err.Error(), "unknown auth") {
		t.Errorf("unknown auth should fail; got %v", err)
	}
}

// --- Framework: Default value application via SkillConfigure ------

func TestSkillConfigure_AppliesDefault(t *testing.T) {
	// gsuite's auth field has Default: "gcloud". Configuring with no
	// auth supplied should apply the default and proceed (not error
	// on the Required check).
	dir := t.TempDir()
	srv := fakeGmailServer(t)
	defer srv.Close()

	// Use the real gsuite catalog entry but override Probe + force the
	// auth Default to "gcloud" for this test (the production catalog
	// defaults to "oauth" now, which requires oauth_creds_json — that's
	// a different test). This test specifically verifies Default
	// APPLICATION, not the production catalog's choice of default.
	originalKind := skillKinds["gsuite"]
	defer func() { skillKinds["gsuite"] = originalKind }()
	k := skillKinds["gsuite"]
	// Override auth.Default to "gcloud" so the test exercises the
	// no-args-supplies-default path without dragging in oauth's
	// required oauth_creds_json field.
	patchedFields := make([]skillKindField, len(k.Fields))
	copy(patchedFields, k.Fields)
	for i, f := range patchedFields {
		if f.Key == "auth" {
			patchedFields[i].Default = "gcloud"
			patchedFields[i].NoteAfter = nil // skip the gcloud-on-PATH check in tests
		}
	}
	k.Fields = patchedFields
	k.Probe = func(ctx context.Context, c *credentials, s map[string]string, _ func() error) (string, error) {
		// Verify Default landed in settings.
		if s["auth"] != "gcloud" {
			return "", errors.New("auth default not applied: settings[auth]=" + s["auth"])
		}
		return "test-identity", nil
	}
	skillKinds["gsuite"] = k

	counter, enabler := newCountingEnabler(1)
	sc := newSkillCfgControl(dir, skillKinds, enabler)

	out, err := sc.SkillConfigure([]string{"gsuite"}, false /* noEnable */)
	if err != nil {
		t.Fatalf("configure with no args should succeed (default applied); got %v", err)
	}
	if !strings.Contains(out, "test-identity") {
		t.Errorf("probe identity not surfaced:\n%s", out)
	}
	if atomic.LoadInt32(counter) != 1 {
		t.Errorf("auto-enable should fire on probe success; got %d calls", atomic.LoadInt32(counter))
	}
	// Verify auth=gcloud landed in config.json.
	lc, _ := loadLaunchConfig(dir)
	if lc.Skills["gsuite"] == nil || lc.Skills["gsuite"].Settings["auth"] != "gcloud" {
		t.Errorf("auth=gcloud should be persisted; got %+v", lc.Skills["gsuite"])
	}
}

// --- Doctor: checkGSuite ------------------------------------------

func TestDoctor_CheckGSuite(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{}}
		r := checkGSuite(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("expected SKIP; got %v", r.Status)
		}
	})
	t.Run("configured but disabled", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"gsuite": {Enabled: false, Settings: map[string]string{"auth": "gcloud"}}},
		}}
		r := checkGSuite(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("expected SKIP when disabled; got %v", r.Status)
		}
	})
	t.Run("enabled with invalid auth mode → FAIL", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{cfgDir: t.TempDir()}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"gsuite": {Enabled: true, Settings: map[string]string{"auth": "totally-bogus"}}},
		}}
		r := checkGSuite(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("invalid auth should be FAIL; got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "invalid") {
			t.Errorf("FAIL detail should say invalid; got %q", r.Detail)
		}
	})
	t.Run("auth=sa missing sa_json → FAIL", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{cfgDir: t.TempDir()}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"gsuite": {Enabled: true, Settings: map[string]string{
				"auth": "sa" /* no sa_json, no subject */}}},
		}}
		r := checkGSuite(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("sa without sa_json should be FAIL; got %v: %q", r.Status, r.Detail)
		}
		if r.Fix == "" {
			t.Error("FAIL should carry a Fix hint")
		}
	})
	t.Run("enabled + gcloud but real probe will fail → WARN", func(t *testing.T) {
		// On a box without gcloud on PATH, the probe returns the
		// "gcloud not found" error. checkGSuite surfaces probe
		// failures as WARN (not FAIL), preserving offline-tolerant
		// behavior.
		e := &doctorEnv{c: &commonFlags{cfgDir: t.TempDir()}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"gsuite": {Enabled: true, Settings: map[string]string{"auth": "gcloud"}}},
		}}
		r := checkGSuite(context.Background(), e)
		// On a dev box with gcloud installed AND ADC done, this returns OK.
		// On CI / minimal box, it returns WARN. Both are valid.
		if r.Status != statusWarn && r.Status != statusOK {
			t.Errorf("expected WARN or OK; got %v: %q", r.Status, r.Detail)
		}
		if r.Status == statusWarn && r.Fix == "" {
			t.Error("WARN should carry a Fix hint")
		}
	})
}
