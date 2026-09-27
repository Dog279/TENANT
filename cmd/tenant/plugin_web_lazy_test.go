package main

import (
	"context"
	"encoding/json"
	"flag"
	reflectpkg "reflect"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/orchestra"
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

func TestReaderEnabled_DefaultOffAndExplicitOptIn(t *testing.T) {
	t.Setenv("TENANT_WEB_READER", "")
	if readerEnabled(nil) {
		t.Fatal("new/unset config must keep remote reader off")
	}
	lc := &launchConfig{Skills: map[string]*skillConfig{
		"web": {Enabled: true, Settings: map[string]string{"jina_reader": "on"}},
	}}
	if !readerEnabled(lc) {
		t.Fatal("persisted jina_reader=on must explicitly enable the reader")
	}
	t.Setenv("TENANT_WEB_READER", "false")
	if readerEnabled(lc) {
		t.Fatal("explicit environment off must override persisted opt-in")
	}
	t.Setenv("TENANT_WEB_READER", "true")
	if !readerEnabled(nil) {
		t.Fatal("explicit environment opt-in must enable the reader")
	}
}

func TestReaderActivityEvent_HostVisibleWithoutTargetURL(t *testing.T) {
	ev := readerActivityEvent(web.ReaderActivity{Provider: "r.jina.ai", TargetHost: "example.com"})
	if ev.Kind != agent.EventToolCall || ev.Tool != "web_reader_remote" {
		t.Fatalf("unexpected audit event: %+v", ev)
	}
	for _, field := range []string{ev.Args, ev.Result} {
		if !strings.Contains(field, "r.jina.ai") || !strings.Contains(field, "example.com") {
			t.Fatalf("audit field must show provider and target host: %q", field)
		}
		if strings.Contains(field, "/private") || strings.Contains(field, "token") {
			t.Fatalf("audit field must not contain target path/query: %q", field)
		}
	}
}

func TestLazyWeb_AdvertisesSpecsWithoutSession(t *testing.T) {
	lw := newLazyWeb(web.Config{}, web.Policy{}, "", nil, nil, nil)
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

func TestLazyWeb_RefusesBeforeLaunchWhenBrowserBudgetExhausted(t *testing.T) {
	called := 0
	lw := newLazyWeb(web.Config{}, web.Policy{}, "", nil, func() (func(), error) {
		called++
		return nil, &orchestra.LimitError{Resource: "browser instances", Used: "1", Limit: "1"}
	}, nil)
	out, isErr, err := lw.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate", Arguments: json.RawMessage(`{"url":"https://example.com"}`)})
	if err != nil || !isErr || called != 1 || !strings.Contains(out, "browser instances") {
		t.Fatalf("budget refusal should be a clean pre-launch tool error: out=%q isErr=%v err=%v called=%d", out, isErr, err, called)
	}
}
