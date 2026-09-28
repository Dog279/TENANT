package main

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/dashboard"
	"tenant/internal/logs"
)

func TestResolveDashboardAuthGeneratesAndPersistsDefault(t *testing.T) {
	dir := t.TempDir()
	first, err := resolveDashboardAuth(dir, "")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if ok, _ := regexp.MatchString(`^[A-Za-z0-9_-]{43}$`, first); !ok {
		t.Fatalf("generated token has unexpected format: %q", first)
	}
	b, err := os.ReadFile(dashboardAuthTokenPath(dir))
	if err != nil {
		t.Fatalf("read persisted token: %v", err)
	}
	if string(b) != first+"\n" {
		t.Fatalf("persisted token = %q, want generated token", b)
	}

	second, err := resolveDashboardAuth(dir, "")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second != first {
		t.Fatalf("token rotated across resolves: %q != %q", second, first)
	}
}

// TEN-332: `tenant serve` and `tenant attach` racing on a fresh install must
// converge on one token. Before, each minted its own and the later rename won,
// so the running server kept a token that no longer matched the file.
func TestResolveDashboardAuthConcurrentCallersAgree(t *testing.T) {
	dir := t.TempDir()
	const callers = 8
	tokens := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = resolveDashboardAuth(dir, "")
		}()
	}
	close(start)
	wg.Wait()

	b, err := os.ReadFile(dashboardAuthTokenPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	persisted := strings.TrimSpace(string(b))
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tokens[i] != persisted {
			t.Fatalf("caller %d got %q, but the persisted token is %q", i, tokens[i], persisted)
		}
	}
}

func TestResolveDashboardAuthExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveDashboardAuth(dir, "configured-token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "configured-token" {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(dashboardAuthTokenPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("explicit token should not create generated token file; stat err=%v", err)
	}
}

func TestResolveDashboardConfigCopiesAllowedHosts(t *testing.T) {
	lc := &launchConfig{Dashboard: dashboardConfig{AllowedHosts: []string{"dash.example:8443"}}}
	cfg, _ := resolveDashboardConfig(lc, false, false, "")
	if len(cfg.AllowedHosts) != 1 || cfg.AllowedHosts[0] != "dash.example:8443" {
		t.Fatalf("allowed hosts = %v", cfg.AllowedHosts)
	}
	lc.Dashboard.AllowedHosts[0] = "mutated"
	if cfg.AllowedHosts[0] != "dash.example:8443" {
		t.Fatal("resolved config aliases launch config allowed hosts")
	}
}

// TEN-360: the dashboard token must never reach the log book. The dashboard
// runs on a hub the way serve and the TUI wire it, takes authenticated
// requests, and its start-up line goes through pushSys; no retained entry and
// no file under the log root may contain the token.
func TestDashboardTokenStaysOutOfTheLogBook(t *testing.T) {
	cfgDir, root := t.TempDir(), t.TempDir()
	token, err := resolveDashboardAuth(cfgDir, "")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	pushSys := func(s string) { hub.Logf("system", slog.LevelInfo, s) }
	mgr := &dashboardManager{
		base:   context.Background(),
		cfg:    dashboard.Config{Addr: "127.0.0.1:0", Auth: token},
		tools:  fakeDashTools{},
		broker: agent.NewBroker(0),
		logs:   hub,
		log:    slog.New(hub.Handler(nil)).With("source", "dashboard"),
		notify: pushSys,
	}
	t.Cleanup(func() { _ = mgr.Shutdown() })
	addr, err := mgr.Enable()
	if err != nil {
		t.Fatal(err)
	}
	pushSys(dashboardServingLine(addr, cfgDir, nil))
	if !healthzUp(addr, 2*time.Second) {
		t.Fatal("dashboard did not come up")
	}
	for _, path := range []string{"/api/activity", "/api/logs", "/api/status", "/ws?token=" + token} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	_ = mgr.Shutdown()

	all, _ := hub.Snapshot()
	var sawLine bool
	for _, e := range all {
		if strings.Contains(e.Line(), token) {
			t.Errorf("retained entry holds the token: %s", e.Line())
		}
		sawLine = sawLine || strings.Contains(e.Msg, dashboardAuthTokenPath(cfgDir))
	}
	if !sawLine {
		t.Errorf("no status line names the token file; ring: %+v", all)
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), token) {
				t.Errorf("%s holds the token", p)
			}
		}
		return nil
	})

	if b := dashboardTokenBanner(token, cfgDir, nil, false); strings.Contains(b, token) || !strings.Contains(b, dashboardAuthTokenPath(cfgDir)) {
		t.Errorf("banner without a terminal = %q; want the token file, not the token", b)
	}
	if b := dashboardTokenBanner(token, cfgDir, nil, true); !strings.Contains(b, token) {
		t.Errorf("banner on a terminal = %q; want the token", b)
	}
	lc := &launchConfig{Dashboard: dashboardConfig{Auth: token}}
	if h := dashboardTokenHint(cfgDir, lc); strings.Contains(h, token) || !strings.Contains(h, "dashboard.auth") {
		t.Errorf("hint for a configured token = %q", h)
	}
}
