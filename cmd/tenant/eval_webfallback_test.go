package main

import (
	"errors"
	"testing"

	"tenant/internal/plugins/wiki"
)

// A web plugin the eval turned on itself that can't start Chrome is dropped:
// the run goes on without it (its tasks are skipped), instead of failing.
func TestBuildEvalToolsRunsWithoutWebWhenChromeWontStart(t *testing.T) {
	pf := &pluginFlags{}
	webFlagsOf(pf).on = true
	calls := 0
	build := func() (*toolMux, *wiki.Index, func(), error) {
		calls++
		if webFlagsOf(pf).on {
			return nil, nil, nil, webStartError{errors.New("start chrome: websocket url timeout reached")}
		}
		return &toolMux{}, nil, func() {}, nil
	}
	mux, _, err := buildEvalTools([]string{"web", "os"}, pf, build)
	if err != nil || mux == nil || calls != 2 || webFlagsOf(pf).on {
		t.Fatalf("auto-enabled web: mux %v, err %v, %d builds, web on %v", mux, err, calls, webFlagsOf(pf).on)
	}

	// Web the operator turned on is their call: the run fails as before.
	pf, calls = &pluginFlags{}, 0
	webFlagsOf(pf).on = true
	if _, _, err := buildEvalTools([]string{"os"}, pf, build); err == nil || calls != 1 {
		t.Errorf("explicit web: err %v, %d builds", err, calls)
	}

	// Other launch failures are not retried.
	calls = 0
	other := func() (*toolMux, *wiki.Index, func(), error) {
		calls++
		return nil, nil, nil, errors.New("sql plugin: database is locked")
	}
	if _, _, err := buildEvalTools([]string{"web"}, &pluginFlags{}, other); err == nil || calls != 1 {
		t.Errorf("other failure: err %v, %d builds", err, calls)
	}
}

// The operator-facing message is unchanged by the typed error.
func TestWebStartErrorKeepsItsMessage(t *testing.T) {
	err := webStartError{errors.New("web: start chrome: websocket url timeout reached")}
	if got, want := err.Error(), "web plugin: web: start chrome: websocket url timeout reached (is Chrome installed?)"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}
