package main

// models_dashctl.go adapts modelControl to dashboard.ModelControl (TEN-204):
// the web Model page reuses the SAME backend registry + live-swap logic the
// TUI /model does, mapping tui.ModelInfo to the dashboard's structured view.
// No business logic forks. Every call is synchronous (no browser), unlike the
// MCP connect flow; adding a server waits for its check.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"tenant/internal/dashboard"
)

type dashModel struct{ mc *modelControl }

func (d dashModel) Models() []dashboard.ModelView {
	in := d.mc.ModelList()
	out := make([]dashboard.ModelView, 0, len(in))
	for _, m := range in {
		v := dashboard.ModelView{
			Name:     m.Name,
			Kind:     m.Kind,
			Model:    m.Model,
			Active:   m.Active,
			Degraded: m.Degraded,
		}
		// A self-hosted server shows where it is, and whether its model is
		// the one it serves (found at each start).
		if pk := providerKinds[m.Kind]; pk.Local && pk.Backend == "vllm" {
			v.Address = m.Endpoint
			if v.Address == safeDisplayEndpoint("") { // saved without one: the kind's own
				v.Address = pk.DefaultEndpoint
			}
			v.AutoModel = m.Model == ""
		}
		out = append(out, v)
	}
	return out
}

// Use switches the active backend (modelOverride "" keeps the saved variant).
func (d dashModel) Use(name string) (string, error) {
	status, _, err := d.mc.UseModel(name, "")
	return status, err
}

func (d dashModel) AddCloud(kind, apiKey string) (string, error) {
	return d.mc.addCloudModel(kind, apiKey, surfaceDashboard)
}

// AddServer checks a server and adds it; a failure the operator can fix
// comes back in the page's words.
func (d dashModel) AddServer(ctx context.Context, req dashboard.ServerRequest) (dashboard.ServerAdded, error) {
	added, err := d.mc.addServer(ctx, serverRequest{Address: req.Address, Model: req.Model, Name: req.Name, APIKey: req.APIKey}, surfaceDashboard)
	if err != nil {
		var pe *plainError
		if errors.As(err, &pe) {
			return dashboard.ServerAdded{}, dashboard.PlainError(pe.plain)
		}
		return dashboard.ServerAdded{}, err
	}
	return dashboard.ServerAdded{
		Name:   added.Name,
		Server: serverKindWords[added.Kind],
		Model:  displayText(added.Model, 80),
		Auto:   added.Auto,
		Tools:  string(added.Tools),
		Format: toolFormatWords[added.ToolFmt],
	}, nil
}

// serverKindWords and toolFormatWords say a server's kind and a tool-call
// format the way the Model page does.
var (
	serverKindWords = map[string]string{
		"vllm": "a vLLM server", "llamacpp": "a llama.cpp server", "ollama": "Ollama",
		"openai-compat": "an OpenAI-compatible server",
	}
	toolFormatWords = map[string]string{
		"qwen": "Qwen (Hermes)", "gemma": "Gemma", "llama": "Llama", "mistral": "Mistral", "glm": "GLM", "openai": "standard OpenAI",
	}
)

func (d dashModel) Remove(name string) (string, error) {
	return d.mc.removeModel(name, surfaceDashboard)
}
func (d dashModel) LoopCeiling() int                     { return d.mc.LoopCeiling() }
func (d dashModel) SetLoopCeiling(n int) (string, error) { return d.mc.SetLoopCeiling(n) }
func (d dashModel) RepeatGuard() int                     { return d.mc.RepeatGuard() }
func (d dashModel) Vision() string                       { return d.mc.Vision() }
func (d dashModel) SetRepeatGuard(n int) (string, error) { return d.mc.SetRepeatGuard(n) }

// Judge reports the Judge's settings for the Model page's card (TEN-510,
// TEN-516): never the key, only whether one is saved.
func (d dashModel) Judge() dashboard.JudgeView {
	v := dashboard.JudgeView{Kind: judgeDefaultKind, Kinds: judgeKindOrder, Rounds: judgeDefaultRounds}
	if d.mc.judge == nil {
		return v
	}
	st := d.mc.judge.Settings()
	v.On, v.Kind, v.Endpoint, v.Model, v.Rounds, v.HasKey = st.on, st.kind, st.endpoint, st.model, st.rounds, st.hasKey
	return v
}

// SetJudge sets up or switches the Judge from the card (TEN-510, TEN-516).
// Rounds given are saved; then, switching on, the loop is pointed at the
// server and the server asked one question first, with the typed key when
// one was given (it is saved only once the server has answered with it;
// blank keeps the saved key). Off saves the address and model without asking.
func (d dashModel) SetJudge(on bool, kind, endpoint, key, model string, rounds int) (string, error) {
	j := d.mc.judge
	if j == nil {
		return "", fmt.Errorf("the judge isn't available here")
	}
	if rounds > 0 {
		if _, err := j.Rounds(rounds); err != nil {
			return "", err
		}
	}
	switch {
	case on && strings.TrimSpace(key) != "":
		return j.use(kind, endpoint, model, key, true, surfaceDashboard)
	case on:
		return j.Use(kind, endpoint, model)
	case strings.TrimSpace(key) != "":
		// Off, with a key typed: keep it for when the judge is switched on.
		if err := j.setKey(key, surfaceDashboard); err != nil {
			return "", err
		}
	}
	return j.store(false, endpoint, model)
}
func (d dashModel) ReloadKeys() (string, error) { return d.mc.ReloadKeys() }
