package mcpremote

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStaticClient_RefusesRedirect locks the TEN-186 security fix: the static
// pairing-token client must NOT follow a redirect, so a malicious peer cannot
// 302 the dial to an attacker host and harvest the long-lived bearer (which
// staticBearer re-injects on every hop, defeating stdlib's cross-host stripping).
func TestStaticClient_RefusesRedirect(t *testing.T) {
	var leaked string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusFound) // 302 → attacker
	}))
	defer peer.Close()

	client := newStaticClient("PAIRING-SECRET", nil)
	resp, err := client.Get(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("redirect must NOT be followed; got status %d (followed to attacker)", resp.StatusCode)
	}
	if leaked != "" {
		t.Errorf("pairing token leaked to the redirect target: %q", leaked)
	}
}

// TestStaticBearer_InjectsToken confirms the bearer is set on the (single,
// non-redirected) request.
func TestStaticBearer_InjectsToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	resp, err := newStaticClient("tok123", nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer tok123" {
		t.Errorf("Authorization = %q, want Bearer tok123", got)
	}
}
