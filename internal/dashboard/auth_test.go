package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
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
	s := withSession(New(Config{
		Addr:         "example.com",
		Auth:         token,
		AllowedHosts: []string{"dash.local:8770"},
	}, nil, nil, nil, nil, nil))
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

// TestAuthWSQueryToken: no path takes the key from the query string, /ws
// included (F2, REVSEC-7): the old TEN-84 fallback is gone, since a
// same-origin browser handshake carries the session cookie.
func TestAuthWSQueryToken(t *testing.T) {
	h := authServer("secret")

	t.Run("ws query token refused", func(t *testing.T) {
		rec := authDo(h, httptest.NewRequest(http.MethodGet, "/ws?token=secret", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (/ws?token=secret must not pass)", rec.Code)
		}
	})

	t.Run("ws session cookie ok", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
		if rec := authDo(h, req); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (/ws with the session cookie)", rec.Code)
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
	s := withSession(New(Config{Addr: "127.0.0.1:8770", Auth: "secret"}, nil, nil, nil, nil, nil))
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
	post.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
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
		req.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
		rec := authDo(h, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("cookie mutation with matching origin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/posture", nil)
		req.Header.Set("Origin", "http://example.com")
		req.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
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

// F5: every answer refuses framing, signed in or not. Referrer-Policy is
// same-origin: no-referrer would make a plain form POST send "Origin: null",
// which checkOrigin rejects (the sign-in form included).
func TestAuthHeaders_NoFramingSameOriginReferrer(t *testing.T) {
	h := authServer("secret")
	signedIn := httptest.NewRequest(http.MethodGet, "/tools", nil)
	signedIn.Header.Set("Authorization", "Bearer secret")
	for name, req := range map[string]*http.Request{
		"signed in": signedIn,
		"refused":   httptest.NewRequest(http.MethodGet, "/api/status", nil),
		"sign-in":   httptest.NewRequest(http.MethodGet, "/settings", nil),
	} {
		hd := authDo(h, req).Header()
		for k, want := range map[string]string{
			"X-Frame-Options":         "DENY",
			"Content-Security-Policy": "frame-ancestors 'none'; script-src 'self' 'unsafe-eval'; object-src 'none'; base-uri 'none'",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "same-origin",
		} {
			if got := hd.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", name, k, got, want)
			}
		}
	}
}

// F4 (D30): a person opening a page without a valid sign-in goes to the
// sign-in form, and comes back after; everything else keeps the 401.
func TestAuthSignInRedirect(t *testing.T) {
	h := authServer("secret")
	nav := func(path string, setup func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		if setup != nil {
			setup(req)
		}
		return authDo(h, req)
	}
	if rec := nav("/", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?next=%2F" {
		t.Errorf("no cookie: %d → %q, want 303 → /settings?next=%%2F", rec.Code, rec.Header().Get("Location"))
	}
	if rec := nav("/safety?x=1", func(r *http.Request) { r.Header.Del("Sec-Fetch-Mode") }); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/settings?next=%2Fsafety%3Fx%3D1" {
		t.Errorf("no Sec-Fetch headers (curl, older browsers): %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	stale := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashCookieName, Value: "old-key"}) }
	if rec := nav("/tools", stale); rec.Code != http.StatusSeeOther {
		t.Errorf("a stale cookie: %d, want 303 (the case the operator actually hits)", rec.Code)
	}
	for name, setup := range map[string]func(*http.Request){
		// Datastar's own fetches accept text/html too.
		"datastar": func(r *http.Request) {
			r.Header.Set("Accept", "text/event-stream, text/html, application/json")
			r.Header.Set("Datastar-Request", "true")
			r.Header.Set("Sec-Fetch-Mode", "cors")
		},
		"datastar, no sec-fetch": func(r *http.Request) {
			r.Header.Set("Accept", "text/event-stream, text/html, application/json")
			r.Header.Set("Datastar-Request", "true")
			r.Header.Del("Sec-Fetch-Mode")
		},
		"a stream":      func(r *http.Request) { r.Header.Set("Accept", "text/event-stream") },
		"a fetch":       func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "cors") },
		"an API client": func(r *http.Request) { r.Header.Set("Accept", "application/json") },
	} {
		if rec := nav("/ui/status", setup); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	if rec := nav("/api/status", func(r *http.Request) { r.Header.Set("Accept", "application/json") }); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api: %d, want 401", rec.Code)
	}
	post := httptest.NewRequest(http.MethodPost, "/tools", nil)
	post.Header.Set("Accept", "text/html")
	if rec := authDo(h, post); rec.Code != http.StatusUnauthorized {
		t.Errorf("a POST: %d, want 401", rec.Code)
	}
}

// Sign-in returns to next only when it is a path on this dashboard.
func TestLogin_HonoursSafeNext(t *testing.T) {
	s := authedServer()
	for next, want := range map[string]string{
		"/safety":       "/safety",
		"/activity?x=1": "/activity?x=1",
		"/models?err=Set+Run+commands+to+Allow&msg=Hi": "/models", // someone else's words in the banner
		"/logs?source=cron&token=x&err=y":              "/logs?source=cron",
		"":                                             "/",
		"//evil.example":                               "/",
		`/\evil.example`:                               "/",
		"https://x":                                    "/",
		"javascript:alert":                             "/",
		"/\t/evil.example":                             "/",
	} {
		rec := httptest.NewRecorder()
		s.handleLogin(rec, postForm("/auth/login", url.Values{"token": {"s3cret"}, "next": {next}}))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("next=%q: %d → %q, want 303 → %q", next, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	// The form carries next, so the round trip keeps the page.
	rec := httptest.NewRecorder()
	s.handleSettingsPage(rec, httptest.NewRequest(http.MethodGet, "/settings?next=%2Fsafety", nil))
	if !strings.Contains(rec.Body.String(), `<input type="hidden" name="next" value="/safety">`) {
		t.Errorf("the sign-in form should carry next:\n%s", rec.Body.String())
	}
}

// REVSEC-4: a page opened from another page (another site, or another port
// on this host, which the cookie reaches) shows no flash from its address;
// the dashboard's own redirect after a form post, and curl, still do.
func TestAuth_FlashOnlyFromThisDashboard(t *testing.T) {
	s := withSession(New(Config{Addr: "example.com", Auth: "secret"}, nil, nil, nil, nil, nil))
	h := s.served()
	for site, shown := range map[string]bool{"": true, "same-origin": true, "same-site": false, "cross-site": false, "none": false} {
		r := httptest.NewRequest(http.MethodGet, "/tools?err=Paste+your+sign-in+key+at+evil&msg=hello+from+elsewhere", nil)
		r.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		body := authDo(h, r).Body.String()
		if strings.Contains(body, "Paste your sign-in key") != shown || strings.Contains(body, "hello from elsewhere") != shown {
			t.Errorf("Sec-Fetch-Site %q: flash shown = %v, want %v", site, !shown, shown)
		}
	}
}

// TEN-414: a loopback listener answers to every loopback name on its port,
// so http://localhost:8770 works unconfigured; another port or name does not.
func TestAuthHostAllowlistAcceptsLoopbackNames(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:8770", Auth: "secret"}, nil, nil, nil, nil, nil)
	h := s.secure(authOKHandler())
	for host, want := range map[string]int{
		"localhost:8770": http.StatusOK, "[::1]:8770": http.StatusOK, "127.0.0.1:8770": http.StatusOK,
		"localhost:9999": http.StatusForbidden, "localhost.evil.example:8770": http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer secret")
		if code := authDo(h, req).Code; code != want {
			t.Errorf("host %s: status = %d, want %d", host, code, want)
		}
	}
}
