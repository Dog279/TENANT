package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// serveStatusServer spins an httptest server whose /api/status returns body.
func serveStatusServer(t *testing.T, body string) (addr string, close func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

func serveEnv(addr string) *doctorEnv {
	return &doctorEnv{
		c:  &commonFlags{},
		lc: &launchConfig{Dashboard: dashboardConfig{Addr: addr}},
	}
}

func TestCheckServe_PendingApprovalsWarn(t *testing.T) {
	addr, done := serveStatusServer(t, `{"status":"ok","turn_active":false,"pending_approvals":2}`)
	defer done()
	r := checkServe(context.Background(), serveEnv(addr))
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
	if !strings.Contains(r.Detail, "2 dangerous-action approval") {
		t.Fatalf("detail = %q", r.Detail)
	}
}

func TestCheckServe_StuckTurnWarn(t *testing.T) {
	addr, done := serveStatusServer(t, `{"status":"ok","turn_active":true,"turn_age_secs":600,"pending_approvals":0}`)
	defer done()
	r := checkServe(context.Background(), serveEnv(addr))
	if r.Status != statusWarn || !strings.Contains(r.Detail, "possibly stuck") {
		t.Fatalf("got %v %q, want WARN/stuck", r.Status, r.Detail)
	}
}

func TestCheckServe_IdleOK(t *testing.T) {
	addr, done := serveStatusServer(t, `{"status":"ok","turn_active":false,"pending_approvals":0}`)
	defer done()
	r := checkServe(context.Background(), serveEnv(addr))
	if r.Status != statusOK || !strings.Contains(r.Detail, "healthy") {
		t.Fatalf("got %v %q, want OK/healthy", r.Status, r.Detail)
	}
}

func TestCheckServe_ActiveButNotStuckOK(t *testing.T) {
	addr, done := serveStatusServer(t, `{"status":"ok","turn_active":true,"turn_age_secs":12,"pending_approvals":0}`)
	defer done()
	r := checkServe(context.Background(), serveEnv(addr))
	if r.Status != statusOK || !strings.Contains(r.Detail, "turn active 12s") {
		t.Fatalf("got %v %q, want OK with active turn", r.Status, r.Detail)
	}
}

func TestCheckServe_UnreachableSkips(t *testing.T) {
	// A port nothing listens on → SKIP (not a fault: daemon simply not running).
	r := checkServe(context.Background(), serveEnv("127.0.0.1:1")) // port 1: refused
	if r.Status != statusSkip {
		t.Fatalf("status = %v, want SKIP when no hub is up (detail=%q)", r.Status, r.Detail)
	}
}

func TestCheckServe_DisabledSkips(t *testing.T) {
	no := false
	e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{Dashboard: dashboardConfig{Enabled: &no}}}
	r := checkServe(context.Background(), e)
	if r.Status != statusSkip {
		t.Fatalf("status = %v, want SKIP when dashboard disabled", r.Status)
	}
}

// A default install keeps the dashboard token in its file, not in
// dashboard.auth: the probe presents it instead of drawing a 401 (TEN-367).
func TestCheckServe_UsesTokenFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer filetok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","turn_active":false,"pending_approvals":0}`))
	}))
	defer srv.Close()
	cfg := t.TempDir()
	if err := os.WriteFile(dashboardAuthTokenPath(cfg), []byte("filetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := serveEnv(strings.TrimPrefix(srv.URL, "http://"))
	e.c = &commonFlags{cfgDir: cfg, auditOnly: true}
	if r := checkServe(context.Background(), e); r.Status != statusOK {
		t.Fatalf("got %v %q, want OK with the token file", r.Status, r.Detail)
	}
}
