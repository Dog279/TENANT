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
	s := New(Config{Addr: "127.0.0.1:0", Auth: token}, nil, nil, nil, nil, nil)
	return s.secure(authOKHandler())
}

// authDo runs one request through h and returns the response recorder.
func authDo(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAuthBindPolicy covers the fail-closed bind guard: loopback always passes;
// a non-loopback bind passes only with BOTH TLS and auth set.
func TestAuthBindPolicy(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"loopback plain", Config{Addr: "127.0.0.1:8770"}, false},
		{"loopback ipv6", Config{Addr: "[::1]:8770"}, false},
		{"localhost", Config{Addr: "localhost:8770"}, false},
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
// GET /healthz exempt, and empty-token passthrough.
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

	t.Run("empty token passthrough", func(t *testing.T) {
		open := authServer("")
		rec := authDo(open, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (no token configured)", rec.Code)
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
// same-origin Origin or no Origin → allowed. Auth is empty so only CORS gates.
func TestAuthCORS(t *testing.T) {
	h := authServer("")

	t.Run("cross-origin rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = "dash.local:8770"
		req.Header.Set("Origin", "http://evil.example:8770")
		rec := authDo(h, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for cross-origin", rec.Code)
		}
	})

	t.Run("same-origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = "dash.local:8770"
		req.Header.Set("Origin", "http://dash.local:8770")
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for same-origin", rec.Code)
		}
	})

	t.Run("no origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = "dash.local:8770"
		rec := authDo(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for origin-less request", rec.Code)
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
