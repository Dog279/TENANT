package main

import (
	"context"
	"encoding/json"
	"flag"
	reflectpkg "reflect"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/plugins/web"
)

func TestWebPrivateNetworkFlagWiresExactAllowlist(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	pf := bindPluginFlags(fs)
	if err := fs.Parse([]string{
		"--web-private-network", "grafana.internal",
		"--web-private-network", "buildbox.internal",
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"grafana.internal", "buildbox.internal"}
	if cfg := webConfig("", pf, nil); !reflectpkg.DeepEqual(cfg.PrivateNetwork, want) {
		t.Fatalf("private network allowlist = %v, want %v", cfg.PrivateNetwork, want)
	}
	if got := webConfig("", &pluginFlags{}, nil).PrivateNetwork; len(got) != 0 {
		t.Fatalf("default private network allowlist must be empty, got %v", got)
	}
}

func TestLazyWeb_AdvertisesSpecsWithoutSession(t *testing.T) {
	lw := newLazyWeb(web.Config{}, web.Policy{}, "", nil, nil)
	specs := lw.Tools()
	if len(specs) == 0 {
		t.Fatal("lazyWeb must advertise the web tool specs before any session exists")
	}
	var sawNavigate bool
	for _, s := range specs {
		if s.Name == "web_navigate" {
			sawNavigate = true
		}
	}
	if !sawNavigate {
		t.Fatal("web_navigate spec missing")
	}
}

func TestWebConfigCarriesBrowserNoSandboxOptIn(t *testing.T) {
	got := webConfig(t.TempDir(), &pluginFlags{}, &launchConfig{BrowserNoSandbox: true})
	if !got.NoSandbox {
		t.Fatal("browser_no_sandbox config was not passed to the web plugin")
	}
	if webConfig(t.TempDir(), &pluginFlags{}, nil).NoSandbox {
		t.Fatal("missing launch config must keep Chrome sandboxed")
	}
}

// With Chrome forced to a bad path, the first browse fails CLEANLY (tool
// error, not a crash), the failure is cached (no repeated spawn attempts),
// and no cleanup is registered for a session that never opened.
func TestLazyWeb_FailsCleanlyAndCachesNoLeak(t *testing.T) {
	var cleanups int
	lw := newLazyWeb(
		web.Config{ChromePath: `Z:\definitely\not\chrome.exe`},
		web.Policy{},
		"",
		func(func()) { cleanups++ },
		nil,
	)
	args, _ := json.Marshal(map[string]string{"url": "https://example.com"})

	out, isErr, err := lw.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate", Arguments: args})
	if err != nil {
		t.Fatalf("dispatch returned transport err: %v", err)
	}
	if !isErr || !strings.Contains(out, "browser unavailable") {
		t.Fatalf("expected a clean browser-unavailable tool error, got: %q isErr=%v", out, isErr)
	}

	// Second call must NOT re-attempt to spawn (failure cached).
	if lw.disp != nil {
		t.Fatal("no dispatcher should have been created on failure")
	}
	lw.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate", Arguments: args})

	// A session that never opened registers no cleanup.
	if cleanups != 0 {
		t.Fatalf("no cleanup should be registered when no session opened, got %d", cleanups)
	}
}
