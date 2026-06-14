package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"tenant/internal/model"
)

type fakePlugin struct {
	name    string
	lastHit *string
}

func (f fakePlugin) Tools() []model.ToolSpec {
	return []model.ToolSpec{{Name: f.name + "_do", Parameters: json.RawMessage(`{}`)}}
}
func (f fakePlugin) Dispatch(_ context.Context, c model.ToolCall) (string, bool, error) {
	*f.lastHit = f.name
	return "ok:" + f.name, false, nil
}

func TestToolMux_MergesAndRoutes(t *testing.T) {
	var hit string
	mux := newToolMux()
	mux.add("a", fakePlugin{name: "a", lastHit: &hit})
	mux.add("b", fakePlugin{name: "b", lastHit: &hit})

	if len(mux.All()) != 2 {
		t.Fatalf("All() = %d, want 2", len(mux.All()))
	}
	out, isErr, _ := mux.Dispatch(context.Background(), model.ToolCall{Name: "b_do"})
	if isErr || out != "ok:b" || hit != "b" {
		t.Fatalf("b_do routed wrong: out=%q isErr=%v hit=%q", out, isErr, hit)
	}
	out, isErr, _ = mux.Dispatch(context.Background(), model.ToolCall{Name: "a_do"})
	if isErr || out != "ok:a" || hit != "a" {
		t.Fatalf("a_do routed wrong: out=%q isErr=%v hit=%q", out, isErr, hit)
	}
	if out, isErr, _ := mux.Dispatch(context.Background(), model.ToolCall{Name: "nope"}); !isErr || out == "" {
		t.Fatalf("unknown tool should error: out=%q isErr=%v", out, isErr)
	}
}

func TestToolMux_DuplicateNameFirstWins(t *testing.T) {
	var h1, h2 string
	mux := newToolMux()
	mux.add("a", fakePlugin{name: "a", lastHit: &h1})
	mux.add("a", fakePlugin{name: "a", lastHit: &h2}) // same tool name "a_do"

	if len(mux.All()) != 1 {
		t.Fatalf("duplicate tool name should not double-register: got %d specs", len(mux.All()))
	}
	mux.Dispatch(context.Background(), model.ToolCall{Name: "a_do"})
	if h1 != "a" || h2 != "" {
		t.Fatalf("first registrant should win: h1=%q h2=%q", h1, h2)
	}
}

// Runtime enable/disable: a disabled tool drops out of Search + Get
// (uncallable, out of the prompt) and Dispatch refuses it; re-enabling
// restores it.
func TestToolMux_RuntimeToggle(t *testing.T) {
	var hit string
	mux := newToolMux()
	mux.add("wiki", fakePlugin{name: "wiki", lastHit: &hit}) // tool "wiki_do"
	mux.add("os", fakePlugin{name: "os", lastHit: &hit})     // tool "os_do"
	ctx := context.Background()

	// Disable a single tool by exact name.
	if n, scope, _ := mux.SetEnabled("os_do", false); n != 1 || scope != "tool" {
		t.Fatalf("SetEnabled(os_do,false) = %d,%q", n, scope)
	}
	// Gone from Search + Get; Dispatch refuses.
	specs, _ := mux.Search(ctx, nil, 0)
	if len(specs) != 1 || specs[0].Name != "wiki_do" {
		t.Fatalf("disabled tool still in Search: %+v", specs)
	}
	if _, ok := mux.Get("os_do"); ok {
		t.Fatal("disabled tool still gettable (would pass validation)")
	}
	if out, isErr, _ := mux.Dispatch(ctx, model.ToolCall{Name: "os_do"}); !isErr {
		t.Fatalf("disabled tool should refuse dispatch: %q", out)
	}

	// Re-enable by plugin label.
	if n, scope, _ := mux.SetEnabled("os", true); n != 1 || scope != "plugin" {
		t.Fatalf("SetEnabled(os,true) = %d,%q", n, scope)
	}
	if _, ok := mux.Get("os_do"); !ok {
		t.Fatal("re-enabled tool not gettable")
	}
	// Unknown target.
	if n, _, _ := mux.SetEnabled("nope", false); n != 0 {
		t.Fatalf("unknown target matched %d", n)
	}
	// ToolList reflects state.
	for _, ti := range mux.ToolList() {
		if ti.Name == "os_do" && (!ti.Enabled || ti.Plugin != "os") {
			t.Fatalf("ToolList wrong for os_do: %+v", ti)
		}
	}
}

// SetPluginEnabled is the explicit categorical toggle — must NEVER
// fall through to tool-name matching, even when the label collides with
// a tool name. This is the safety contract that lets `/enable skill X`
// give a clean "no skill named X" error on typos.
func TestToolMux_SetPluginEnabled_PluginScopeOnly(t *testing.T) {
	var hit string
	mux := newToolMux()
	mux.add("wiki", fakePlugin{name: "wiki", lastHit: &hit})
	mux.add("os", fakePlugin{name: "os", lastHit: &hit})

	// Plugin sweep — toggles both tools at the label, returns (N, "plugin", nil).
	if n, scope, err := mux.SetPluginEnabled("os", false); err != nil || n != 1 || scope != "plugin" {
		t.Fatalf("SetPluginEnabled(os,false) = (%d, %q, %v); want (1, plugin, nil)", n, scope, err)
	}
	// "os_do" is a tool name, not a plugin label — must NOT match.
	if n, scope, _ := mux.SetPluginEnabled("os_do", false); n != 0 || scope != "" {
		t.Fatalf("SetPluginEnabled must reject tool names: got (%d, %q); want (0, \"\")", n, scope)
	}
	// Re-enable by plugin label.
	if n, _, _ := mux.SetPluginEnabled("os", true); n != 1 {
		t.Fatalf("SetPluginEnabled(os,true) = %d; want 1", n)
	}
}

// Plugins() returns the sorted, deduped set of plugin labels — drives
// the "did you mean" hint when `/enable skill <typo>` finds nothing.
func TestToolMux_Plugins_SortedAndDeduped(t *testing.T) {
	var hit string
	mux := newToolMux()
	// Register in non-alphabetical order to verify sorting.
	mux.add("wiki", fakePlugin{name: "wiki", lastHit: &hit})
	mux.add("os", fakePlugin{name: "os", lastHit: &hit})
	mux.add("sql", fakePlugin{name: "sql", lastHit: &hit})

	got := mux.Plugins()
	want := []string{"os", "sql", "wiki"}
	if len(got) != len(want) {
		t.Fatalf("Plugins() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Plugins()[%d] = %q; want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// Regression: Search must surface EVERY enabled tool, even when there
// are more than the profile's MaxToolsPerCall and the wanted one was
// registered last. This is the bug where sql+wiki+os filled a cap of 12
// and `/enable web` (registered later) never reached the prompt, so the
// model truthfully said it had no browser.
func TestToolMux_SearchReturnsAllEnabledIgnoringCap(t *testing.T) {
	mux := newToolMux()
	// 13 tools registered before "web", mirroring sql(3)+wiki(5)+os(5).
	for i := 0; i < 13; i++ {
		var hit string
		mux.add("early", fakePlugin{name: "early" + strconv.Itoa(i), lastHit: &hit})
	}
	var hit string
	mux.add("web", fakePlugin{name: "web", lastHit: &hit}) // 14th tool

	// Old code truncated to k=12 by order; web_do would be dropped.
	got, _ := mux.Search(context.Background(), nil, 12)
	var sawWeb bool
	for _, s := range got {
		if s.Name == "web_do" {
			sawWeb = true
		}
	}
	if !sawWeb {
		t.Fatalf("web_do dropped by cap; Search returned %d tools without it", len(got))
	}
	if len(got) != 14 {
		t.Fatalf("Search should return all 14 enabled tools, got %d", len(got))
	}
}

// Lazy activation: a stubbed config-free plugin (web) builds its real
// dispatcher the first time it's enabled, swapping the stub owner, and
// only once. This is what lets `/enable web` spawn Chrome at runtime
// instead of forcing a relaunch with --web.
func TestToolMux_LazyActivation(t *testing.T) {
	ctx := context.Background()
	mux := newToolMux()
	// Stub stands in for the unconfigured plugin: advertises the spec but
	// returns a "needs setup" status until activated.
	mux.add("web", stubPlugin{specs: []model.ToolSpec{{Name: "web_do", Parameters: json.RawMessage(`{}`)}}, hint: "stubbed"})
	mux.SetEnabled("web", false)

	var hit string
	var builds, cleanups int
	mux.registerActivator("web", func() (plugin, func(), error) {
		builds++
		return fakePlugin{name: "web", lastHit: &hit}, func() { cleanups++ }, nil
	})

	// Before activation, the stub answers.
	if out, _, _ := mux.Dispatch(ctx, model.ToolCall{Name: "web_do"}); hit != "" || out == "ok:web" {
		t.Fatalf("stub should answer before activation: out=%q", out)
	}

	// /enable web activates for real and swaps the owner.
	if n, scope, err := mux.SetEnabled("web", true); err != nil || n != 1 || scope != "plugin" {
		t.Fatalf("enable web: n=%d scope=%q err=%v", n, scope, err)
	}
	if out, isErr, _ := mux.Dispatch(ctx, model.ToolCall{Name: "web_do"}); isErr || out != "ok:web" || hit != "web" {
		t.Fatalf("after activation, real plugin should answer: out=%q isErr=%v hit=%q", out, isErr, hit)
	}

	// Toggling again must not rebuild.
	mux.SetEnabled("web", false)
	if _, _, err := mux.SetEnabled("web", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if builds != 1 {
		t.Fatalf("activator should run once, ran %d times", builds)
	}

	// Close runs the activation cleanup.
	mux.Close()
	if cleanups != 1 {
		t.Fatalf("activation cleanup should run once on Close, ran %d", cleanups)
	}
}

// A failed lazy activation (e.g. Chrome won't launch) surfaces the error
// and leaves the tool disabled rather than half-enabling it.
func TestToolMux_LazyActivationError(t *testing.T) {
	mux := newToolMux()
	mux.add("web", stubPlugin{specs: []model.ToolSpec{{Name: "web_do", Parameters: json.RawMessage(`{}`)}}, hint: "stubbed"})
	mux.SetEnabled("web", false)
	mux.registerActivator("web", func() (plugin, func(), error) {
		return nil, nil, context.DeadlineExceeded // stand-in for "Chrome not found"
	})

	n, _, err := mux.SetEnabled("web", true)
	if err == nil {
		t.Fatal("activation failure should return an error")
	}
	if n != 0 {
		t.Fatalf("nothing should toggle on failure, got n=%d", n)
	}
	if _, ok := mux.Get("web_do"); ok {
		t.Fatal("tool should remain disabled after failed activation")
	}
}

// During restore, a plugin whose activator fails (e.g. gsuite configured
// with an unsupported auth) backs ALL its tools through one shared
// activator. restore must attempt activation ONCE and emit ONE note — not
// re-run the failing activator and spam an identical line per tool — while
// still restoring unrelated plugins. Regression: a stale auth="composio" in
// config.json produced nine identical "unknown auth" lines at every launch,
// one per enabled gsuite tool.
func TestToolMux_RestoreDedupesPluginActivationFailure(t *testing.T) {
	m := newToolMux()
	// gsuite-like: one stub advertising several tools, all owned by the
	// "gsuite" label, behind an activator that fails (unsupported auth).
	m.add("gsuite", stubPlugin{specs: []model.ToolSpec{
		{Name: "gmail_send", Parameters: json.RawMessage(`{}`)},
		{Name: "drive_read", Parameters: json.RawMessage(`{}`)},
		{Name: "calendar_create", Parameters: json.RawMessage(`{}`)},
	}, hint: "stub"})
	m.SetEnabled("gsuite", false)
	// A working sibling proves restore still applies other plugins.
	var hit string
	m.add("os", fakePlugin{name: "os", lastHit: &hit}) // os_do

	builds := 0
	m.registerActivator("gsuite", func() (plugin, func(), error) {
		builds++
		return nil, nil, fmt.Errorf("gsuite plugin: auth %q isn't supported in this build", "composio")
	})

	notes := m.restore(map[string]bool{
		"gmail_send": true, "drive_read": true, "calendar_create": true,
		"os_do": true,
	})

	if builds != 1 {
		t.Fatalf("activator should run once for the whole plugin, ran %d times", builds)
	}
	failNotes := 0
	for _, n := range notes {
		if strings.Contains(n, "isn't supported") {
			failNotes++
		}
	}
	if failNotes != 1 {
		t.Fatalf("expected exactly ONE activation-failure note, got %d: %v", failNotes, notes)
	}
	for _, name := range []string{"gmail_send", "drive_read", "calendar_create"} {
		if _, ok := m.Get(name); ok {
			t.Fatalf("%s should stay disabled after failed activation", name)
		}
	}
	if _, ok := m.Get("os_do"); !ok {
		t.Fatal("working sibling os_do should restore enabled")
	}
}

// A plugin the operator hasn't configured yet (activator returns an
// errNotConfigured-wrapped error) must restore SILENTLY — no "could not
// restore" line at launch — while still attempting activation only once and
// leaving its tools disabled. This is the "stop nagging me about gsuite
// until I set up SA" path. Contrast TestToolMux_RestoreDedupesPluginActivationFailure,
// where a genuine failure still gets one note.
func TestToolMux_RestoreSilentWhenNotConfigured(t *testing.T) {
	m := newToolMux()
	m.add("gsuite", stubPlugin{specs: []model.ToolSpec{
		{Name: "gmail_send", Parameters: json.RawMessage(`{}`)},
		{Name: "drive_read", Parameters: json.RawMessage(`{}`)},
		{Name: "calendar_create", Parameters: json.RawMessage(`{}`)},
	}, hint: "stub"})
	m.SetEnabled("gsuite", false)

	builds := 0
	m.registerActivator("gsuite", func() (plugin, func(), error) {
		builds++
		// Mirror the production activator's wrap: clean message, sentinel cause.
		return nil, nil, fmt.Errorf("gsuite plugin: %w yet — run `/configure gsuite`", errNotConfigured)
	})

	notes := m.restore(map[string]bool{
		"gmail_send": true, "drive_read": true, "calendar_create": true,
	})

	if builds != 1 {
		t.Fatalf("activator should run once even when not configured, ran %d times", builds)
	}
	if len(notes) != 0 {
		t.Fatalf("an unconfigured plugin must restore silently, got notes: %v", notes)
	}
	for _, name := range []string{"gmail_send", "drive_read", "calendar_create"} {
		if _, ok := m.Get(name); ok {
			t.Fatalf("%s should stay disabled when the plugin isn't configured", name)
		}
	}
}

// TEN-225: RankingStatus surfaces WHY a Search surfaced what it did, so a silent
// fallback to the full enabled catalog (the cause of a fat tool dump every turn)
// is visible instead of hidden. Zero selection-behavior change.
func TestToolMux_RankingStatus_Diagnostic(t *testing.T) {
	ctx := context.Background()

	// Below the ranking threshold → inactive, with a clear reason.
	small := newToolMux()
	for i := 0; i < 5; i++ {
		small.add("p", fakePlugin{name: "tool" + strconv.Itoa(i)})
	}
	if _, err := small.Search(ctx, []float32{1, 0}, 12); err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranked, _, catalog, reason, ok := small.RankingStatus()
	if !ok {
		t.Fatal("RankingStatus must be set after a Search")
	}
	if ranked {
		t.Error("a sub-threshold catalog must not rank")
	}
	if catalog != 5 {
		t.Errorf("catalog=%d, want 5", catalog)
	}
	if !strings.Contains(reason, "threshold") {
		t.Errorf("reason should cite the threshold, got %q", reason)
	}

	// Above threshold but NO embedder installed → full catalog surfaced, and the
	// reason names the root cause (the operator's most likely case).
	big := newToolMux()
	for i := 0; i < 25; i++ {
		big.add("p", fakePlugin{name: "tool" + strconv.Itoa(i)})
	}
	if _, err := big.Search(ctx, []float32{1, 0}, 12); err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranked, surfaced, catalog, reason, ok := big.RankingStatus()
	if !ok || ranked {
		t.Errorf("no embedder → ranking OFF; got ranked=%v ok=%v", ranked, ok)
	}
	if surfaced != 25 || catalog != 25 {
		t.Errorf("full catalog expected: surfaced=%d catalog=%d, want 25/25", surfaced, catalog)
	}
	if !strings.Contains(reason, "embedder") {
		t.Errorf("reason should cite the missing embedder, got %q", reason)
	}
}
