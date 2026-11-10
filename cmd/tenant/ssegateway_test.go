package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A loopback gateway answers to loopback names on its own port and to
// nothing else: any other name is one a web page could have pointed here.
func TestLoopbackGatewayHost(t *testing.T) {
	cases := []struct {
		host, bindPort string
		want           bool
	}{
		{"127.0.0.1:8765", "8765", true},
		{"localhost:8765", "8765", true},
		{"LOCALHOST:8765", "8765", true}, // a host name has no case
		{"[::1]:8765", "8765", true},
		{"127.0.0.2:8765", "8765", true}, // all of 127/8 is this machine
		{"127.0.0.1:9999", "8765", false},
		{"127.0.0.1", "8765", false},
		{"127.0.0.1:55001", "0", true}, // an OS-assigned port
		{"other.example:8765", "8765", false},
		{"127.0.0.1.other.example:8765", "8765", false},
		{"192.168.1.10:8765", "8765", false},
		{"", "8765", false},
	}
	for _, c := range cases {
		if got := loopbackGatewayHost(c.host, c.bindPort); got != c.want {
			t.Errorf("loopbackGatewayHost(%q, %q) = %v, want %v", c.host, c.bindPort, got, c.want)
		}
	}
}

func TestGatewayOriginAllowed(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:8765", "127.0.0.1:8765", true},
		{"http://localhost:8765", "127.0.0.1:8765", false}, // another name is another origin
		{"http://127.0.0.1:9000", "127.0.0.1:8765", false}, // another port is another site's page
		{"https://127.0.0.1:8765", "127.0.0.1:8765", false},
		{"http://other.example", "127.0.0.1:8765", false},
		{"null", "127.0.0.1:8765", false},
		{"http://127.0.0.1:8765/x", "127.0.0.1:8765", false},
		{"http://user@127.0.0.1:8765", "127.0.0.1:8765", false},
	}
	for _, c := range cases {
		if got := gatewayOriginAllowed(c.origin, c.host); got != c.want {
			t.Errorf("gatewayOriginAllowed(%q, %q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

// The guard in front of a handler: what a non-browser client sends passes;
// what a page can make a browser send does not reach the handler.
func TestGuardSSEGateway(t *testing.T) {
	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusNoContent) })
	const token = "the-gateway-token"
	serve := func(bind, host, origin string) int {
		r := httptest.NewRequest(http.MethodGet, "/sse", nil)
		r.Host = host
		r.Header.Set("Authorization", "Bearer "+token) // the page cases fail whatever they hold
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		guardSSEGateway(bind, token, next).ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		name, bind, host, origin string
		want                     int
	}{
		{"a client, no Origin", "127.0.0.1:8765", "127.0.0.1:8765", "", http.StatusNoContent},
		{"a client by name", "127.0.0.1:8765", "localhost:8765", "", http.StatusNoContent},
		{"a page on another site", "127.0.0.1:8765", "127.0.0.1:8765", "http://other.example", http.StatusForbidden},
		{"a page on another local port", "127.0.0.1:8765", "127.0.0.1:8765", "http://127.0.0.1:3000", http.StatusForbidden},
		{"a sandboxed page", "127.0.0.1:8765", "127.0.0.1:8765", "null", http.StatusForbidden},
		{"a re-pointed name", "127.0.0.1:8765", "other.example:8765", "", http.StatusForbidden},
		{"a re-pointed name, same-origin to itself", "127.0.0.1:8765", "other.example:8765", "http://other.example:8765", http.StatusForbidden},
		{"an overlay bind by its network name", "100.64.0.5:8765", "box.overlay.example:8765", "", http.StatusNoContent},
		{"an overlay bind, a page on another site", "100.64.0.5:8765", "box.overlay.example:8765", "http://other.example", http.StatusForbidden},
	} {
		before := reached
		got := serve(c.bind, c.host, c.origin)
		if got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
		if (got == http.StatusForbidden) == (reached != before) {
			t.Errorf("%s: handler reached = %v with status %d", c.name, reached != before, got)
		}
	}
}

// The token: only `Authorization: Bearer <the token>` passes. Nothing else
// reaches the handler, and a gateway built with no token refuses everything
// rather than accepting an empty one.
func TestGuardSSEGateway_Token(t *testing.T) {
	const token = "the-gateway-token"
	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusNoContent) })
	serve := func(guardToken, target, authorization string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Host = "127.0.0.1:8765"
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		guardSSEGateway("127.0.0.1:8765", guardToken, next).ServeHTTP(w, r)
		return w
	}
	for _, c := range []struct{ name, guardToken, target, authorization string }{
		{"no header", token, "/sse", ""},
		{"another token", token, "/sse", "Bearer another-token"},
		{"a prefix of the token", token, "/sse", "Bearer the-gateway"},
		{"the token without the scheme", token, "/sse", token},
		{"another scheme", token, "/sse", "Basic " + token},
		{"the scheme in another case", token, "/sse", "bearer " + token},
		{"the token in the URL", token, "/sse?token=" + token, ""},
		{"an empty bearer", token, "/sse", "Bearer "},
		{"an empty bearer against a gateway with no token", "", "/sse", "Bearer "},
		{"no header against a gateway with no token", "", "/sse", ""},
	} {
		before := reached
		w := serve(c.guardToken, c.target, c.authorization)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: status %d, WWW-Authenticate %q; want 401 and Bearer", c.name, w.Code, w.Header().Get("WWW-Authenticate"))
		}
		if reached != before {
			t.Errorf("%s: the handler was reached", c.name)
		}
		if strings.Contains(w.Body.String(), token) {
			t.Errorf("%s: the refusal names the token", c.name)
		}
	}
	if w := serve(token, "/sse", "Bearer "+token); w.Code != http.StatusNoContent || reached != 1 {
		t.Fatalf("the token itself: status %d, handler reached %d times; want 204 and once", w.Code, reached)
	}
}

// The token file: made on first use, owner-only content of one random token,
// the same token on every later read, and never an empty one.
func TestResolveGatewayAuth(t *testing.T) {
	cfg := t.TempDir()
	first, err := resolveGatewayAuth(cfg)
	if err != nil || len(first) < 32 {
		t.Fatalf("first token = %q, %v; want a long random token", first, err)
	}
	second, err := resolveGatewayAuth(cfg)
	if err != nil || second != first {
		t.Fatalf("second read = %q, %v; want the first token again", second, err)
	}
	if other, _ := resolveGatewayAuth(t.TempDir()); other == first {
		t.Fatal("two installs were given the same token")
	}
	if dash, _ := resolveDashboardAuth(cfg, ""); dash == first {
		t.Fatal("the gateway's token is the dashboard's")
	}
	if err := os.WriteFile(gatewayAuthTokenPath(cfg), []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveGatewayAuth(cfg); err == nil {
		t.Fatalf("an empty token file gave token %q and no error", got)
	}
	// The file tools must never read it, and doctor must audit it.
	managed, _ := managedSecrets(cfg, "")
	found := false
	for _, m := range managed {
		found = found || m.path == gatewayAuthTokenPath(cfg)
	}
	if !found {
		t.Fatal("gateway-auth-token is not on the managed secrets list")
	}
}

// On the wire, against a running bridge: a request without the token, or
// with a wrong one, gets 401 on the stream and on the message route alike; a
// request that names the bridge by another host, or carries another site's
// Origin, is refused even with the token; and an ordinary client still gets a
// session after all of them.
func TestMCPBridge_RefusesWhatItShould(t *testing.T) {
	addr := startBridge(t)
	token := bridgeToken(t, addr)
	do := func(method, path, host, origin, authorization string) int {
		req, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(http.MethodGet, "/sse", "", "", ""); got != http.StatusUnauthorized {
		t.Errorf("stream without a token = %d, want 401", got)
	}
	if got := do(http.MethodGet, "/sse", "", "", "Bearer not-the-token"); got != http.StatusUnauthorized {
		t.Errorf("stream with a wrong token = %d, want 401", got)
	}
	if got := do(http.MethodPost, "/message", "", "", ""); got != http.StatusUnauthorized {
		t.Errorf("message without a token = %d, want 401", got)
	}
	_, port, _ := strings.Cut(addr, ":")
	if got := do(http.MethodGet, "/sse", "other.example:"+port, "", "Bearer "+token); got != http.StatusForbidden {
		t.Errorf("another Host, with the token = %d, want 403", got)
	}
	if got := do(http.MethodGet, "/sse", "", "http://other.example", "Bearer "+token); got != http.StatusForbidden {
		t.Errorf("another site's Origin, with the token = %d, want 403", got)
	}
	if tools := oneSession(t, addr); len(tools) == 0 {
		t.Fatal("an ordinary client got no session after the refusals")
	}
}
