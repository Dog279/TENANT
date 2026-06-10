package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type fakeSecrets struct {
	items    []SecretItem
	setCalls []string // "credID=value"
}

func (f *fakeSecrets) List() []SecretItem     { return f.items }
func (f *fakeSecrets) Set(id, v string) error { f.setCalls = append(f.setCalls, id+"="+v); return nil }
func (f *fakeSecrets) Remove(id string) error { return nil }

func newConfModel(s SecretsControl) *model {
	m := newModel(context.Background(), Config{Secrets: s})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func TestConfigure_OpensPicker(t *testing.T) {
	fs := &fakeSecrets{items: []SecretItem{
		{CredID: "openai", Name: "OpenAI", Category: "LLM provider"},
		{CredID: "tavily", Name: "Tavily", Category: "Web search", Set: true},
	}}
	m := newConfModel(fs)
	m.handleSlash("/configure")
	if m.picker == nil {
		t.Fatal("/configure (no arg) should open the arrow-key picker")
	}
	if len(m.picker.items) != 2 {
		t.Fatalf("picker items=%d want 2", len(m.picker.items))
	}
}

func TestConfigure_PickArmsMaskedEntryThenSaves(t *testing.T) {
	fs := &fakeSecrets{items: []SecretItem{{CredID: "openai", Name: "OpenAI", Category: "LLM provider"}}}
	m := newConfModel(fs)
	m.handleSlash("/configure")

	// Select the first item.
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.secretEntry == nil || m.secretEntry.credID != "openai" {
		t.Fatalf("after select, secretEntry=%+v", m.secretEntry)
	}
	if m.input.EchoMode != textinput.EchoPassword {
		t.Error("input must be masked during secret entry")
	}
	if m.picker != nil {
		t.Error("picker should clear after selection")
	}

	// Save the key.
	m.saveSecretEntry("sk-abc123")
	if len(fs.setCalls) != 1 || fs.setCalls[0] != "openai=sk-abc123" {
		t.Fatalf("Set not called correctly: %v", fs.setCalls)
	}
	if m.secretEntry != nil {
		t.Error("secretEntry should clear after save")
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Error("input should unmask after save")
	}
}

func TestConfigure_EmptyValueCancels(t *testing.T) {
	fs := &fakeSecrets{items: []SecretItem{{CredID: "openai", Name: "OpenAI", Category: "LLM provider"}}}
	m := newConfModel(fs)
	m.handleSlash("/configure")
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.saveSecretEntry("   ") // blank → no save
	if len(fs.setCalls) != 0 {
		t.Errorf("blank value must not call Set: %v", fs.setCalls)
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Error("blank entry should still unmask")
	}
}

func TestConfigure_CancelClearsSecretEntry(t *testing.T) {
	fs := &fakeSecrets{items: []SecretItem{{CredID: "openai", Name: "OpenAI", Category: "LLM provider"}}}
	m := newConfModel(fs)
	m.handleSlash("/configure")
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.handleSlash("/cancel")
	if m.secretEntry != nil {
		t.Error("/cancel should clear secretEntry")
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Error("/cancel should unmask the input")
	}
}

// Trap A: Esc during masked entry must cancel + unmask (operator not stranded).
func TestConfigure_EscDuringEntryUnmasks(t *testing.T) {
	fs := &fakeSecrets{items: []SecretItem{{CredID: "openai", Name: "OpenAI", Category: "LLM provider"}}}
	m := newConfModel(fs)
	m.handleSlash("/configure")
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.secretEntry == nil || m.input.EchoMode != textinput.EchoPassword {
		t.Fatal("precondition: should be armed + masked")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.secretEntry != nil {
		t.Error("Esc should clear secretEntry")
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Error("Esc should unmask the input")
	}
}

// Trap B: a slash command while armed aborts the entry + unmasks (not stuck).
func TestConfigure_SlashWhileArmedAborts(t *testing.T) {
	fs := &fakeSecrets{items: []SecretItem{{CredID: "openai", Name: "OpenAI", Category: "LLM provider"}}}
	m := newConfModel(fs)
	m.handleSlash("/configure")
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.input.SetValue("/help")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // submit a slash while armed
	if m.secretEntry != nil {
		t.Error("a slash command while armed should abort the entry")
	}
	if m.input.EchoMode != textinput.EchoNormal {
		t.Error("input should unmask after the slash aborts entry")
	}
	if len(fs.setCalls) != 0 {
		t.Errorf("the slash must not be stored as a key: %v", fs.setCalls)
	}
}

// The pasted key must never appear in the chat transcript.
func TestConfigure_KeyNeverEchoedToChat(t *testing.T) {
	fs := &fakeSecrets{items: []SecretItem{{CredID: "openai", Name: "OpenAI", Category: "LLM provider"}}}
	m := newConfModel(fs)
	m.handleSlash("/configure")
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	const secret = "sk-SUPERSECRET-DO-NOT-ECHO-9999"
	m.saveSecretEntry(secret)
	for _, msg := range m.msgs {
		if strings.Contains(msg.content, secret) {
			t.Fatalf("secret leaked into chat transcript: %q", msg.content)
		}
	}
	if len(fs.setCalls) != 1 || fs.setCalls[0] != "openai="+secret {
		t.Errorf("Set should still receive the real value: %v", fs.setCalls)
	}
}

func TestConfigure_UnavailableWithoutControls(t *testing.T) {
	m := newModel(context.Background(), Config{}) // no Secrets, no SkillConfig
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/configure")
	if m.picker != nil {
		t.Error("no controls → no picker")
	}
	last := m.msgs[len(m.msgs)-1].content
	if last == "" {
		t.Error("expected an 'unavailable' notice")
	}
}
