package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/plugins/web"
)

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
