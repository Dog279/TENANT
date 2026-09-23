package dashboard

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// authOKHandler is the protected handler under test: it always 200s, so any
// non-200 the test sees came from the secure() envelope, not the handler.
func authOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// authServer builds a Server with the given auth token (TLS/addr irrelevant to
// the middleware) wrapped by secure().
func authServer(token string) http.Handler {
	s := New(Config{
		Addr:         "example.com",
		Auth:         token,
		AllowedHosts: []string{"dash.local:8770"},
	}, nil, nil, nil, nil, nil)
	return s.secure(authOKHandler())
}

// authDo runs one request through h and returns the response recorder.
func authDo(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAuthBindPolicy covers the fail-closed bind guard: every listener needs
// auth, while a non-loopback bind additionally needs TLS.
func TestAuthBindPolicy(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"loopback no auth", Config{Addr: "127.0.0.1:8770"}, true},
		{"loopback plain with auth", Config{Addr: "127.0.0.1:8770", Auth: "tok"}, false},
		{"loopback ipv6 with auth", Config{Addr: "[::1]:8770", Auth: "tok"}, false},
		{"localhost with auth", Config{Addr: "localhost:8770", Auth: "tok"}, false},
		{"wildcard no tls/auth", Config{Addr: "0.0.0.0:8770"}, true},
		{"wildcard tls only", Config{Addr: "0.0.0.0:8770", TLSCert: "c.pem", TLSKey: "k.pem"}, true},
		{"wildcard auth only", Config{Addr: "0.0.0.0:8770", Auth: "tok"}, true},
		{"wildcard tls+auth", Config{Addr: "0.0.0.0:8770", TLSCert: "c.pem", TLSKey: "k.pem", Auth: "tok"}, false},
		{"lan ip no tls/auth", Config{Addr: "192.168.1.50:8770"}, true},
		{"lan ip tls+auth", Config{Addr: "192.168.1.50:8770", TLSCert: "c.pem", TLSKey: "k.pem", Auth: "tok"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg, nil, nil, nil, nil, nil)
			err := s.checkBindPolicy()
			if tc.wantErr && err == nil {
				t.Fatalf("checkBindPolicy(%+v) = nil, want error", tc.cfg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkBindPolicy(%+v) = %v, want nil", tc.cfg, err)
			}
		})
	}
}

// TestAuthBearer covers token enforcement: missing/wrong → 401, correct → 200,
// GET /healthz exempt, and an empty configured token fails closed.
func TestAuthBearer(t *testing.T) {
	h := authServer("secret")

	t.Run("missing token", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Header.Set("Authorization", "Bearer nope")
		rec := authDo(h, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("correct token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("healthz exempt", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (healthz must be exempt)", rec.Code)
		}
	})

	t.Run("empty token fails closed", func(t *testing.T) {
		open := authServer("")
		rec := authDo(open, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (no token configured)", rec.Code)
		}
	})
}

// TestAuthWSQueryToken covers the /ws-only `?token=` fallback (TEN-84):
// browsers can't set the Authorization header on a WS handshake, so /ws also
// accepts the bearer token from the query string. The fallback is scoped to
// /ws — REST paths must NOT honor a query token.
func TestAuthWSQueryToken(t *testing.T) {
	h := authServer("secret")

	t.Run("ws query token ok", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/ws?token=secret", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (/ws?token=secret must pass)", rec.Code)
		}
	})

	t.Run("ws wrong query token", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/ws?token=wrong", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (/ws?token=wrong)", rec.Code)
		}
	})

	t.Run("ws no token no header", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/ws", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (/ws with no token)", rec.Code)
		}
	})

	t.Run("ws header still works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (/ws with Bearer header)", rec.Code)
		}
	})

	t.Run("rest query token not honored", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/api/tools?token=secret", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (query token must NOT be honored for REST)", rec.Code)
		}
	})

	t.Run("healthz still exempt", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (healthz must be exempt)", rec.Code)
		}
	})
}

// TestAuthCORS covers same-origin policy: a cross-origin Origin → 403; a
// same-origin Origin or no Origin → allowed for an authenticated read.
func TestAuthCORS(t *testing.T) {
	h := authServer("secret")

	t.Run("cross-origin rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = "dash.local:8770"
		req.Header.Set("Origin", "http://evil.example:8770")
		req.Header.Set("Authorization", "Bearer secret")
		rec := authDo(h, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for cross-origin", rec.Code)
		}
	})

	t.Run("same-origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = "dash.local:8770"
		req.Header.Set("Origin", "http://dash.local:8770")
		req.Header.Set("Authorization", "Bearer secret")
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for same-origin", rec.Code)
		}
	})

	t.Run("no origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = "dash.local:8770"
		req.Header.Set("Authorization", "Bearer secret")
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for origin-less request", rec.Code)
		}
	})
}

func TestAuthHostAllowlistRejectsRebinding(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:8770", Auth: "secret"}, nil, nil, nil, nil, nil)
	h := s.secure(authOKHandler())

	t.Run("unknown host on loopback listener", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Host = "evil.example:8770"
		rec := authDo(h, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("rebinding shaped origin and host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = "attacker.example:8770"
		req.Header.Set("Origin", "http://attacker.example:8770")
		req.Header.Set("Authorization", "Bearer secret")
		rec := authDo(h, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

// TEN-332: `tailscale serve` forwards the browser's Host, the tailnet name. A
// host trusted at runtime must pass the allowlist, /settings included, and
// stop passing once cleared.
func TestAuthTrustedHostsForTailscaleServe(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:8770", Auth: "secret"}, nil, nil, nil, nil, nil)
	h := s.secure(authOKHandler())
	get := func(path, host string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer secret")
		return authDo(h, req).Code
	}
	const tailnet = "node.tailnet-name.ts.net"

	if code := get("/settings", tailnet); code != http.StatusForbidden {
		t.Fatalf("untrusted tailnet host: status = %d, want 403", code)
	}
	s.SetTrustedHosts([]string{tailnet})
	for _, host := range []string{tailnet, tailnet + ":443", "NODE.Tailnet-Name.ts.net"} {
		for _, path := range []string{"/settings", "/api/status"} {
			if code := get(path, host); code != http.StatusOK {
				t.Errorf("GET %s with Host %s: status = %d, want 200", path, host, code)
			}
		}
	}
	// A cookie-authenticated form post through the proxy carries the tailnet
	// Origin and must still pass the CSRF check.
	post := httptest.NewRequest(http.MethodPost, "/posture", nil)
	post.Host = tailnet
	post.Header.Set("Origin", "https://"+tailnet)
	post.AddCookie(&http.Cookie{Name: dashCookieName, Value: "secret"})
	if code := authDo(h, post).Code; code != http.StatusOK {
		t.Errorf("proxied form post: status = %d, want 200", code)
	}
	if code := get("/api/status", "evil.example"); code != http.StatusForbidden {
		t.Errorf("trusting the tailnet name widened the allowlist: status = %d, want 403", code)
	}

	s.SetTrustedHosts(nil)
	if code := get("/settings", tailnet); code != http.StatusForbidden {
		t.Fatalf("cleared tailnet host: status = %d, want 403", code)
	}
}

func TestAuthCSRF(t *testing.T) {
	h := authServer("secret")

	t.Run("cookie mutation without origin rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/posture", nil)
		req.AddCookie(&http.Cookie{Name: dashCookieName, Value: "secret"})
		rec := authDo(h, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("cookie mutation with matching origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/posture", nil)
		req.Header.Set("Origin", "http://example.com")
		req.AddCookie(&http.Cookie{Name: dashCookieName, Value: "secret"})
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("bearer mutation remains origin independent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/posture", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

// TestAuthGenerateToken: non-empty, distinct across calls, URL-safe charset.
func TestAuthGenerateToken(t *testing.T) {
	a := GenerateToken()
	b := GenerateToken()
	if a == "" || b == "" {
		t.Fatal("GenerateToken returned empty")
	}
	if a == b {
		t.Fatalf("two tokens identical: %q", a)
	}
	urlSafe := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	if !urlSafe.MatchString(a) {
		t.Fatalf("token not URL-safe: %q", a)
	}
}
