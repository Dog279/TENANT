package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeModels struct {
	infos    []ModelInfo
	models   map[string][]string
	listErr  map[string]error
	useErr   error
	useCalls []string // "name|model"
}

func (f *fakeModels) ModelList() []ModelInfo { return f.infos }
func (f *fakeModels) UseModel(name, model string) (string, string, error) {
	f.useCalls = append(f.useCalls, name+"|"+model)
	if f.useErr != nil {
		return "", "", f.useErr
	}
	return "✓ switched to " + name + " " + model, name, nil
}
func (f *fakeModels) ListProviderModels(name string) ([]string, error) {
	if f.listErr != nil {
		if e := f.listErr[name]; e != nil {
			return nil, e
		}
	}
	return f.models[name], nil
}
func (f *fakeModels) AddModel(string, string, string) (string, error) { return "", nil }
func (f *fakeModels) AddCloudModel(string, string) (string, error)    { return "", nil }
func (f *fakeModels) RemoveModel(string) (string, error)              { return "", nil }
func (f *fakeModels) ReloadKeys() (string, error)                     { return "", nil }
func (f *fakeModels) LoopCeiling() int                                { return 0 }
func (f *fakeModels) SetLoopCeiling(int) (string, error)              { return "", nil }

func newModelPickerModel(f *fakeModels) *model {
	m := newModel(context.Background(), Config{Models: f})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func runMsg(c tea.Cmd) tea.Msg {
	if c == nil {
		return nil
	}
	return c()
}

// applyPicker sets m.picker from a pickerStartMsg (mirrors Update's handling).
func applyPicker(t *testing.T, m *model, msg tea.Msg) *listPicker {
	t.Helper()
	ps, ok := msg.(pickerStartMsg)
	if !ok {
		t.Fatalf("expected pickerStartMsg, got %T: %+v", msg, msg)
	}
	m.picker = ps.picker
	return ps.picker
}

func selectItem(p *listPicker, item string) {
	for i, it := range p.items {
		if it == item {
			p.selected = i
			return
		}
	}
}

func TestModelPicker_ZeroProviders(t *testing.T) {
	m := newModelPickerModel(&fakeModels{})
	if cmd := m.startModelPicker(); cmd != nil {
		t.Fatal("zero providers should not open a picker")
	}
	if m.picker != nil {
		t.Fatal("no picker should be set")
	}
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "no model providers configured") {
		t.Fatalf("expected guidance, got %q", last)
	}
}

func TestModelPicker_TwoProviders_FullFlow(t *testing.T) {
	f := &fakeModels{
		infos: []ModelInfo{
			{Name: "zai", Model: "glm-4.6", Active: true},
			{Name: "openai", Model: "gpt-4"},
		},
		models: map[string][]string{"openai": {"gpt-4", "gpt-4o", "o1"}},
	}
	m := newModelPickerModel(f)

	// STATE 1: provider picker.
	prov := applyPicker(t, m, runMsg(m.startModelPicker()))
	if !strings.Contains(strings.ToLower(prov.title), "pick a provider") {
		t.Fatalf("provider picker title = %q", prov.title)
	}
	if len(prov.items) != 2 {
		t.Fatalf("provider items = %d, want 2", len(prov.items))
	}
	if !strings.Contains(prov.currentMark, "zai") {
		t.Errorf("active provider should be marked current, got %q", prov.currentMark)
	}

	// Select openai → STATE 2 fetch → STATE 3 model picker.
	for _, it := range prov.items {
		if strings.HasPrefix(it, "openai") {
			selectItem(prov, it)
		}
	}
	mdl := applyPicker(t, m, runMsg(m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})))
	if !strings.Contains(mdl.title, "Pick a model for openai") {
		t.Fatalf("model picker title = %q", mdl.title)
	}
	if strings.Join(mdl.items, ",") != "gpt-4,gpt-4o,o1" {
		t.Fatalf("model items = %v", mdl.items)
	}

	// Pick gpt-4o → STATE 4 swap.
	selectItem(mdl, "gpt-4o")
	swapMsg := runMsg(m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter}))
	if sc, ok := swapMsg.(sysChatMsg); !ok || !strings.Contains(sc.text, "switched to openai gpt-4o") {
		t.Fatalf("swap msg = %+v", swapMsg)
	}
	if len(f.useCalls) != 1 || f.useCalls[0] != "openai|gpt-4o" {
		t.Fatalf("UseModel calls = %v, want [openai|gpt-4o]", f.useCalls)
	}
}

func TestModelPicker_SingleProvider_SkipsProviderPick(t *testing.T) {
	f := &fakeModels{
		infos:  []ModelInfo{{Name: "zai", Model: "glm-4.6", Active: true}},
		models: map[string][]string{"zai": {"glm-4.6", "glm-5.1"}},
	}
	m := newModelPickerModel(f)
	// One provider → startModelPicker returns the fetch cmd directly (no provider pick).
	mdl := applyPicker(t, m, runMsg(m.startModelPicker()))
	if !strings.Contains(mdl.title, "Pick a model for zai") {
		t.Fatalf("expected model picker for the sole provider, title = %q", mdl.title)
	}
}

func TestModelPicker_FetchErrorFailsClosed(t *testing.T) {
	f := &fakeModels{
		infos:   []ModelInfo{{Name: "anthropic", Model: "", Active: true}},
		listErr: map[string]error{"anthropic": errors.New("HTTP 401")},
	}
	m := newModelPickerModel(f)
	msg := runMsg(m.startModelPicker()) // single provider → fetch directly
	sc, ok := msg.(sysChatMsg)
	if !ok || !strings.Contains(sc.text, "could not fetch models from anthropic") {
		t.Fatalf("expected a fail-closed sysChatMsg, got %+v", msg)
	}
	if len(f.useCalls) != 0 {
		t.Fatalf("no swap should happen on fetch error, got %v", f.useCalls)
	}
}

func TestModelPicker_EmptyListSwitchesToDefault(t *testing.T) {
	f := &fakeModels{
		infos:  []ModelInfo{{Name: "zai", Model: "glm-4.6", Active: true}},
		models: map[string][]string{"zai": {}},
	}
	m := newModelPickerModel(f)
	msg := runMsg(m.startModelPicker())
	if sc, ok := msg.(sysChatMsg); !ok || !strings.Contains(sc.text, "saved default") {
		t.Fatalf("empty list should switch to default, got %+v", msg)
	}
	if len(f.useCalls) != 1 || f.useCalls[0] != "zai|" {
		t.Fatalf("expected UseModel(zai, \"\"), got %v", f.useCalls)
	}
}
