package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type fakeSetup struct {
	view     SetupView
	provOpts []SetupOption
	calls    []string // "Set<X>:value"
}

func (f *fakeSetup) Snapshot() SetupView            { return f.view }
func (f *fakeSetup) ProviderOptions() []SetupOption { return f.provOpts }
func (f *fakeSetup) ToolFormatOptions() []string    { return []string{"qwen", "openai"} }
func (f *fakeSetup) SetProvider(k string) (string, error) {
	f.calls = append(f.calls, "SetProvider:"+k)
	return "switched to " + k, nil
}
func (f *fakeSetup) SetModel(v string) (string, error) {
	f.calls = append(f.calls, "SetModel:"+v)
	return "model set", nil
}
func (f *fakeSetup) SetEndpoint(v string) (string, error) {
	f.calls = append(f.calls, "SetEndpoint:"+v)
	return "endpoint set", nil
}
func (f *fakeSetup) SetToolFormat(v string) (string, error) {
	f.calls = append(f.calls, "SetToolFormat:"+v)
	return "fmt set", nil
}
func (f *fakeSetup) SetKey(v string) (string, error) {
	f.calls = append(f.calls, "SetKey:"+v)
	return "key stored", nil
}
func (f *fakeSetup) SetEmbeddings(ep, m string) (string, error) {
	f.calls = append(f.calls, "SetEmbeddings:"+ep+"/"+m)
	return "embed set", nil
}
func (f *fakeSetup) SetGateway(mode, addr string) (string, error) {
	f.calls = append(f.calls, "SetGateway:"+mode+"/"+addr)
	return "gw set", nil
}

func newSetupModel(f SetupControl) *model {
	m := newModel(context.Background(), Config{Setup: f})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func fullSetupView() SetupView {
	return SetupView{
		Provider: "zai", ProviderLabel: "Z.ai", Model: "glm-4.6", Endpoint: "https://x",
		ToolFormat: "openai", KeySet: true, KeySource: "stored", Gateway: "local",
		NeedsKey: true, NeedsEndpoint: true, IsVLLM: true,
	}
}

func TestSetup_OpensMenu(t *testing.T) {
	f := &fakeSetup{view: fullSetupView()}
	m := newSetupModel(f)
	m.handleSlash("/setup")
	if m.picker == nil {
		t.Fatal("/setup should open the menu picker")
	}
	// provider, key, model, endpoint, tool format, embeddings, gateway = 7
	if len(m.picker.items) != 7 {
		t.Fatalf("menu rows=%d want 7: %v", len(m.picker.items), m.picker.items)
	}
	joined := strings.Join(m.picker.items, "\n")
	for _, want := range []string{"Provider", "API key", "Model", "Endpoint", "Tool format", "Embeddings", "MCP gateway"} {
		if !strings.Contains(joined, want) {
			t.Errorf("menu missing %q", want)
		}
	}
}

func TestSetup_MenuOmitsKeyAndEndpointForEcho(t *testing.T) {
	f := &fakeSetup{view: SetupView{Provider: "echo", Gateway: "local"}}
	m := newSetupModel(f)
	m.handleSlash("/setup")
	joined := strings.Join(m.picker.items, "\n")
	if strings.Contains(joined, "API key") || strings.Contains(joined, "Endpoint") {
		t.Errorf("echo provider should not show key/endpoint rows: %v", m.picker.items)
	}
}

func TestSetup_PickProvider(t *testing.T) {
	f := &fakeSetup{view: fullSetupView(), provOpts: []SetupOption{{Label: "openai — OpenAI", Value: "openai"}}}
	m := newSetupModel(f)
	m.setupPickProvider()
	if m.picker == nil || len(m.picker.items) != 1 {
		t.Fatal("provider sub-picker not opened")
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter}) // pick openai
	if len(f.calls) != 1 || f.calls[0] != "SetProvider:openai" {
		t.Fatalf("SetProvider not called: %v", f.calls)
	}
	if m.picker == nil { // menu should reopen after the edit
		t.Error("menu should reopen after setting provider")
	}
}

func TestSetup_MaskedKeyEntryNotEchoed(t *testing.T) {
	f := &fakeSetup{view: fullSetupView()}
	m := newSetupModel(f)
	m.setupEnterKey()
	if m.setupEntry == nil || !m.setupEntry.masked || m.input.EchoMode != textinput.EchoPassword {
		t.Fatal("key entry should arm a masked setupEntry")
	}
	const secret = "sk-SETUP-SECRET-NOECHO"
	m.input.SetValue(secret)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(f.calls) != 1 || f.calls[0] != "SetKey:"+secret {
		t.Fatalf("SetKey not called with value: %v", f.calls)
	}
	for _, msg := range m.msgs {
		if strings.Contains(msg.content, secret) {
			t.Fatalf("key leaked into chat: %q", msg.content)
		}
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Error("input should unmask after key save")
	}
	if m.picker == nil {
		t.Error("menu should reopen after key save")
	}
}

func TestSetup_TextEntryModel(t *testing.T) {
	f := &fakeSetup{view: fullSetupView()}
	m := newSetupModel(f)
	m.setupEnterModel()
	if m.setupEntry == nil || m.setupEntry.masked {
		t.Fatal("model entry should be an unmasked setupEntry")
	}
	m.input.SetValue("glm-5.1")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(f.calls) != 1 || f.calls[0] != "SetModel:glm-5.1" {
		t.Fatalf("SetModel not called: %v", f.calls)
	}
}

func TestSetup_EscDuringEntryCancels(t *testing.T) {
	f := &fakeSetup{view: fullSetupView()}
	m := newSetupModel(f)
	m.setupEnterKey()
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.setupEntry != nil {
		t.Error("Esc should clear setupEntry")
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Error("Esc should unmask")
	}
	if len(f.calls) != 0 {
		t.Errorf("Esc must not call any setter: %v", f.calls)
	}
}

func TestSetup_Unavailable(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/setup")
	if m.picker != nil {
		t.Error("no Setup control → no menu")
	}
}
