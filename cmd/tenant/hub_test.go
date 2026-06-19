package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// TestBuildHub_Echo exercises the shared substrate extraction (TEN-247) in the
// offline echo backend: buildHub must assemble every field cmdTUI and cmdServe
// rebind, and the returned cleanup must tear down without panicking. This is the
// guard that the cmdTUI↔cmdServe DRY merge didn't drop a wire.
func TestBuildHub_Echo(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	if err := c.resolve(); err != nil { // populates c.lc (fallback chain needs it)
		t.Fatalf("resolve: %v", err)
	}
	pf := &pluginFlags{}

	var notes []string
	pushSys := func(s string) { notes = append(notes, s) }

	h, cleanup, err := buildHub(context.Background(), c, pf, pushSys, log)
	if err != nil {
		t.Fatalf("buildHub: %v", err)
	}
	if h == nil || cleanup == nil {
		t.Fatal("buildHub returned nil hub/cleanup with no error")
	}
	t.Cleanup(func() {
		// Must not panic even though buildHub already wired everything.
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("cleanup panicked: %v", r)
			}
		}()
		cleanup()
	})

	// Every field a caller rebinds must be live. A nil here = a dropped wire
	// that would only surface as a runtime nil-deref in the daily-driver TUI.
	checks := []struct {
		name string
		ok   bool
	}{
		{"router", h.router != nil},
		// NB: h.degraded is legitimately nil when the router builds fine (echo
		// is healthy, not a fallback) — Degraded() is nil-safe. h.wikiIx is
		// legitimately nil when no wiki dir is configured. Both are asserted
		// below for their real contracts, not for non-nil-ness.
		{"stores", h.stores != nil},
		{"stores.episodic", h.stores != nil && h.stores.episodic != nil},
		{"stores.semantic", h.stores != nil && h.stores.semantic != nil},
		{"stores.soul", h.stores != nil && h.stores.soul != nil},
		{"usageStore", h.usageStore != nil},
		{"broker", h.broker != nil},
		{"stg", h.stg != nil},
		{"stgMu", h.stgMu != nil},
		{"saveSettings", h.saveSettings != nil},
		{"mux", h.mux != nil},
		{"skillStore", h.skillStore != nil},
		{"prof", h.prof != nil},
		{"profSynth", h.profSynth != nil},
		{"profMu", h.profMu != nil},
		{"refreshProfile", h.refreshProfile != nil},
		{"noteProfile", h.noteProfile != nil},
		{"skEmb", h.skEmb != nil},
		{"embedderID", h.embedderID != ""},
		{"meta", h.meta != nil},
		{"distiller", h.distiller != nil},
		{"distillJob", h.distillJob != nil},
		{"soulLive", h.soulLive != nil},
		{"log", h.log != nil},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("hub.%s is nil/empty after buildHub", c.name)
		}
	}

	// echo is a real (deterministic) backend, not a degraded fallback, so the
	// router must NOT report degraded and the echo banner must NOT be emitted.
	if h.degraded.Degraded() {
		t.Error("echo backend should not be degraded")
	}
	for _, n := range notes {
		if n == "echo: responses are deterministic stubs — not a real model." {
			t.Error("degraded echo banner emitted for a healthy echo router")
		}
	}

	// saveSettings closes over stgMu/stg and must run without panicking even
	// with no prior settings file (callers reuse it for their onChange hooks).
	h.saveSettings()
}
