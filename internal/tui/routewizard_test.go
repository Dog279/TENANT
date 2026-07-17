package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func wizardInfo() RouteConfigureInfo {
	return RouteConfigureInfo{
		Cloud: []ModelInfo{
			{Name: "anthro", Kind: "anthropic", Model: "claude-x"},
			{Name: "zai", Kind: "zai", Model: "glm-4.6"},
		},
		Local: []ModelInfo{
			{Name: "oll", Kind: "ollama", Model: "qwen-small"},
			{Name: "vllm", Kind: "vllm", Model: "qwen-big", Active: true},
		},
		BigRole: "zai", Mode: "shadow",
		NeverCloud: true, MinComplexityForBig: 4, StickySession: true,
	}
}

func newWizardModel(f *fakeRouteControl, fm *fakeModels) *model {
	m := newModel(context.Background(), Config{Route: f, Models: fm})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

func TestRouteConfigure_FullFlow(t *testing.T) {
	f := &fakeRouteControl{cfgInfo: wizardInfo(), status: "router status card"}
	fm := &fakeModels{models: map[string][]string{
		"anthro": {"claude-a", "claude-b"},
		"vllm":   {"qwen-big", "qwen-huge"},
	}}
	m := newWizardModel(f, fm)

	cmd := m.handleSlash("/route configure")
	if cmd == nil {
		t.Fatal("configure should return a tea.Cmd")
	}
	if f.cfgCalls != 0 {
		t.Fatal("ConfigureInfo ran on the UI goroutine")
	}

	// Step ① provider picker: two cloud providers, current = BigRole (zai).
	w, ok := cmd().(routeWizardMsg)
	if !ok || w.next == nil {
		t.Fatalf("configure msg = %#v, want routeWizardMsg with picker", w)
	}
	if len(w.next.items) != 2 {
		t.Fatalf("cloud provider rows = %v", w.next.items)
	}
	if !strings.Contains(w.next.items[1], "zai") || w.next.currentMark != w.next.items[1] {
		t.Fatalf("current cloud mark = %q items=%v", w.next.currentMark, w.next.items)
	}

	// Pick anthro → live model picker for it.
	w = w.next.onSelect(w.next.items[0])().(routeWizardMsg)
	if got := w.next.items; !reflect.DeepEqual(got, []string{"claude-a", "claude-b"}) {
		t.Fatalf("big model rows = %v", got)
	}

	// Pick claude-b → SetBig applied, small provider picker opens. The apply
	// must run inside the returned cmd (tea.Cmd goroutine), never inline in
	// onSelect on the UI goroutine.
	applyBig := w.next.onSelect("claude-b")
	if len(f.bigs) != 0 {
		t.Fatal("SetBig ran on the UI goroutine")
	}
	w = applyBig().(routeWizardMsg)
	if want := [][2]string{{"anthro", "claude-b"}}; !reflect.DeepEqual(f.bigs, want) {
		t.Fatalf("SetBig calls = %v, want %v", f.bigs, want)
	}
	if !strings.Contains(w.text, "route big anthro/claude-b") {
		t.Fatalf("step ① status = %q", w.text)
	}
	if len(w.next.items) != 2 || w.next.currentMark == "" || !strings.Contains(w.next.currentMark, "vllm") {
		t.Fatalf("small provider rows = %v mark=%q", w.next.items, w.next.currentMark)
	}

	// Pick the active local provider → its live model picker.
	w = w.next.onSelect(w.next.currentMark)().(routeWizardMsg)
	if got := w.next.items; !reflect.DeepEqual(got, []string{"qwen-big", "qwen-huge"}) {
		t.Fatalf("small model rows = %v", got)
	}

	// Pick qwen-huge → UseModel applied, policy menu opens (fresh info).
	cfgCallsBefore := f.cfgCalls
	applySmall := w.next.onSelect("qwen-huge")
	if len(fm.useCalls) != 0 {
		t.Fatal("UseModel ran on the UI goroutine")
	}
	w = applySmall().(routeWizardMsg)
	if want := []string{"vllm|qwen-huge"}; !reflect.DeepEqual(fm.useCalls, want) {
		t.Fatalf("UseModel calls = %v, want %v", fm.useCalls, want)
	}
	if f.cfgCalls != cfgCallsBefore+1 {
		t.Fatal("policy menu should re-read ConfigureInfo")
	}
	if len(w.next.items) != 5 || w.next.items[4] != routePolicyDone {
		t.Fatalf("policy rows = %v", w.next.items)
	}
	if !strings.Contains(w.next.items[0], "shadow") || !strings.Contains(w.next.items[1], "local only") {
		t.Fatalf("policy rows missing current values: %v", w.next.items)
	}
	menu := w.next

	// mode row → value picker → pick "on" → SetMode + menu reopens.
	ps, ok := menu.onSelect(menu.items[0])().(pickerStartMsg)
	if !ok {
		t.Fatal("mode row should open a value picker")
	}
	if ps.picker.currentMark == "" || !strings.HasPrefix(ps.picker.currentMark, "shadow") {
		t.Fatalf("mode mark = %q", ps.picker.currentMark)
	}
	// Esc on a value picker must return to the policy menu, not end the wizard.
	if back, ok := ps.picker.onCancel()().(pickerStartMsg); !ok || !strings.Contains(back.picker.title, "policy") {
		t.Fatalf("value-picker esc = %#v, want reopened policy menu", back)
	}
	applyMode := ps.picker.onSelect(ps.picker.items[3])
	if len(f.modes) != 0 {
		t.Fatal("SetMode ran on the UI goroutine")
	}
	w = applyMode().(routeWizardMsg)
	if want := []string{"on"}; !reflect.DeepEqual(f.modes, want) {
		t.Fatalf("SetMode calls = %v, want %v", f.modes, want)
	}
	if !strings.Contains(w.text, "route mode on") || w.next == nil {
		t.Fatalf("mode apply = %+v", w)
	}
	menu = w.next

	// cloud offload row → pick "allowed" → SetPolicy(never_cloud, false).
	ps = menu.onSelect(menu.items[1])().(pickerStartMsg)
	w = ps.picker.onSelect(ps.picker.items[0])().(routeWizardMsg)
	if want := [][2]string{{"never_cloud", "false"}}; !reflect.DeepEqual(f.policies, want) {
		t.Fatalf("SetPolicy calls = %v, want %v", f.policies, want)
	}
	menu = w.next

	// min complexity row → pick "3".
	ps = menu.onSelect(menu.items[2])().(pickerStartMsg)
	w = ps.picker.onSelect(ps.picker.items[2])().(routeWizardMsg)
	if want := [2]string{"min_complexity_for_big", "3"}; f.policies[len(f.policies)-1] != want {
		t.Fatalf("SetPolicy calls = %v, want last %v", f.policies, want)
	}
	menu = w.next

	// sticky row → pick "off".
	ps = menu.onSelect(menu.items[3])().(pickerStartMsg)
	w = ps.picker.onSelect(ps.picker.items[1])().(routeWizardMsg)
	if want := [2]string{"sticky_session", "false"}; f.policies[len(f.policies)-1] != want {
		t.Fatalf("SetPolicy calls = %v, want last %v", f.policies, want)
	}
	menu = w.next

	// done → live status + test hints.
	sc, ok := menu.onSelect(routePolicyDone)().(sysChatMsg)
	if !ok {
		t.Fatal("done should post a sysChatMsg")
	}
	for _, want := range []string{"router status card", "/route why", "/route stats", "!big"} {
		if !strings.Contains(sc.text, want) {
			t.Errorf("finish output missing %q: %q", want, sc.text)
		}
	}
}

func TestRouteConfigure_SingleCloudSkipsProviderPick(t *testing.T) {
	info := wizardInfo()
	info.Cloud = info.Cloud[:1] // anthro only
	f := &fakeRouteControl{cfgInfo: info}
	fm := &fakeModels{models: map[string][]string{"anthro": {"claude-a"}}}
	m := newWizardModel(f, fm)

	w := m.handleSlash("/route configure")().(routeWizardMsg)
	if w.next == nil || !strings.Contains(w.next.title, "anthro") {
		t.Fatalf("single-cloud step = %+v, want anthro model picker", w)
	}
	if !reflect.DeepEqual(w.next.items, []string{"claude-a"}) {
		t.Fatalf("model rows = %v", w.next.items)
	}
}

func TestRouteConfigure_NoCloudProviders(t *testing.T) {
	f := &fakeRouteControl{cfgInfo: RouteConfigureInfo{Local: wizardInfo().Local}}
	m := newWizardModel(f, &fakeModels{})

	sc, ok := m.handleSlash("/route configure")().(sysChatMsg)
	if !ok || !strings.Contains(sc.text, "/model add-cloud") {
		t.Fatalf("no-cloud output = %#v", sc)
	}
	if len(f.bigs) != 0 {
		t.Fatal("SetBig must not run without a cloud provider")
	}
}

func TestRouteConfigure_BigFetchFailureFastPathDoesNotWrite(t *testing.T) {
	// Single cloud provider = no pick has happened yet, so a fetch failure
	// must not write route.big_role — config changes require an operator
	// decision, not network weather.
	info := wizardInfo()
	info.Cloud = info.Cloud[:1] // anthro only
	f := &fakeRouteControl{cfgInfo: info}
	fm := &fakeModels{listErr: map[string]error{"anthro": errors.New("boom")}}
	m := newWizardModel(f, fm)

	w := m.handleSlash("/route configure")().(routeWizardMsg)
	if len(f.bigs) != 0 {
		t.Fatalf("fast path wrote the big leg without a pick: %v", f.bigs)
	}
	if !strings.Contains(w.text, "could not fetch") || !strings.Contains(w.text, "big leg unchanged") {
		t.Fatalf("fast-path note = %q", w.text)
	}
	if w.next == nil || !strings.Contains(w.next.title, "small") {
		t.Fatalf("fast path should continue to step ②, got %+v", w.next)
	}
}

func TestRouteConfigure_BigFetchFailureAfterPickKeepsSavedModel(t *testing.T) {
	// After an explicit provider pick, a fetch failure falls back to the
	// provider's saved model (SetBig with "").
	f := &fakeRouteControl{cfgInfo: wizardInfo()} // two cloud providers
	fm := &fakeModels{listErr: map[string]error{"anthro": errors.New("boom")}}
	m := newWizardModel(f, fm)

	w := m.handleSlash("/route configure")().(routeWizardMsg)
	w = w.next.onSelect(w.next.items[0])().(routeWizardMsg) // pick anthro
	if want := [][2]string{{"anthro", ""}}; !reflect.DeepEqual(f.bigs, want) {
		t.Fatalf("SetBig calls = %v, want saved-model fallback %v", f.bigs, want)
	}
	if !strings.Contains(w.text, "kept its saved model") || w.next == nil {
		t.Fatalf("fallback step = %+v, want note + small-leg picker", w)
	}
}

func TestRouteConfigure_SetBigErrorContinuesToSmallStep(t *testing.T) {
	f := &fakeRouteControl{cfgInfo: wizardInfo(), bigErr: errors.New("no key resolves")}
	fm := &fakeModels{models: map[string][]string{"anthro": {"claude-a"}}}
	m := newWizardModel(f, fm)

	w := m.handleSlash("/route configure")().(routeWizardMsg)
	w = w.next.onSelect(w.next.items[0])().(routeWizardMsg) // pick anthro
	w = w.next.onSelect("claude-a")().(routeWizardMsg)      // apply fails
	if !strings.Contains(w.text, "no key resolves") || !strings.Contains(w.text, "big leg unchanged") {
		t.Fatalf("SetBig error text = %q", w.text)
	}
	if w.next == nil || !strings.Contains(w.next.title, "small") {
		t.Fatal("SetBig failure must continue to step ②, not end the wizard")
	}
}

func TestRouteConfigure_SingleLocalEmptyListDoesNotSwitch(t *testing.T) {
	// Esc-only walkthrough with one local provider whose endpoint serves no
	// models: the wizard must NOT auto-switch the active planner (that write
	// would persist an unbuildable provider for the next launch).
	info := wizardInfo()
	info.Local = info.Local[:1] // oll only
	f := &fakeRouteControl{cfgInfo: info}
	fm := &fakeModels{} // every fetch: empty list, nil error
	m := newWizardModel(f, fm)

	w := m.handleSlash("/route configure")().(routeWizardMsg)
	w = w.next.onCancel()().(routeWizardMsg) // esc at cloud provider picker
	if len(fm.useCalls) != 0 {
		t.Fatalf("no-pick walkthrough switched the planner: %v", fm.useCalls)
	}
	if !strings.Contains(w.text, "small leg unchanged") {
		t.Fatalf("fast-path note = %q", w.text)
	}
	if w.next == nil || !strings.Contains(w.next.title, "policy") {
		t.Fatalf("should continue to step ③, got %+v", w.next)
	}
}

func TestRouteConfigure_PickedLocalEmptyListSwitchesToSavedDefault(t *testing.T) {
	f := &fakeRouteControl{cfgInfo: wizardInfo()} // two locals
	fm := &fakeModels{}                           // empty lists everywhere
	m := newWizardModel(f, fm)

	w := m.handleSlash("/route configure")().(routeWizardMsg)
	w = w.next.onCancel()().(routeWizardMsg)                // esc big step
	w = w.next.onSelect(w.next.items[0])().(routeWizardMsg) // pick oll explicitly
	if want := []string{"oll|"}; !reflect.DeepEqual(fm.useCalls, want) {
		t.Fatalf("UseModel calls = %v, want explicit-pick default switch %v", fm.useCalls, want)
	}
	if w.next == nil || !strings.Contains(w.next.title, "policy") {
		t.Fatalf("should continue to step ③, got %+v", w.next)
	}
}

func TestRouteConfigure_EscSkipsWithoutRollback(t *testing.T) {
	f := &fakeRouteControl{cfgInfo: wizardInfo(), status: "router status card"}
	fm := &fakeModels{}
	m := newWizardModel(f, fm)

	w := m.handleSlash("/route configure")().(routeWizardMsg)
	// Esc on the cloud provider picker → straight to the small-leg step.
	w = w.next.onCancel()().(routeWizardMsg)
	if !strings.Contains(w.text, "big leg unchanged") || !strings.Contains(w.next.title, "small") {
		t.Fatalf("esc step = %+v", w)
	}
	// Esc again → policy menu; esc there → finish. Nothing was mutated.
	w = w.next.onCancel()().(routeWizardMsg)
	if !strings.Contains(w.next.title, "policy") {
		t.Fatalf("esc → %q, want policy menu", w.next.title)
	}
	sc := w.next.onCancel()().(sysChatMsg)
	if !strings.Contains(sc.text, "router status card") {
		t.Fatalf("finish = %q", sc.text)
	}
	if len(f.bigs) != 0 || len(f.policies) != 0 || len(fm.useCalls) != 0 {
		t.Fatal("esc-only walkthrough must not mutate anything")
	}
}

func TestUpdate_RouteWizardMsgOpensPickerAndPostsText(t *testing.T) {
	m := newWizardModel(&fakeRouteControl{}, &fakeModels{})
	p := &listPicker{title: "t", items: []string{"a"}}

	m.Update(routeWizardMsg{text: "applied", next: p})

	if m.picker != p {
		t.Fatal("wizard msg did not install the picker")
	}
	if got := m.msgs[len(m.msgs)-1].content; got != "applied" {
		t.Fatalf("wizard text = %q", got)
	}
}

func TestUpdate_RouteWizardMsgDoesNotClobberActivePicker(t *testing.T) {
	// A wizard step lands asynchronously; if the operator opened another
	// picker during the fetch gap, the step is dropped, not swapped in under
	// their cursor.
	m := newWizardModel(&fakeRouteControl{}, &fakeModels{})
	userPicker := &listPicker{title: "user", items: []string{"x"}}
	m.picker = userPicker

	m.Update(routeWizardMsg{text: "applied", next: &listPicker{title: "wiz", items: []string{"y"}}})

	if m.picker != userPicker {
		t.Fatal("wizard msg clobbered the operator's active picker")
	}
	if got := m.msgs[len(m.msgs)-1].content; !strings.Contains(got, "another picker") {
		t.Fatalf("clobber notice = %q", got)
	}
}
