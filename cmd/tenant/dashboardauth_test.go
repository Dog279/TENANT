package main

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
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
