package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tenant/internal/logs/event/eventtest"
	"tenant/internal/model"
)

// eventData is the text of each field of the first event id of provider.
func eventData(rec *eventtest.Recorder, provider string, id uint32) map[string]string {
	for _, e := range rec.Events() {
		if e.Provider == provider && e.ID == id {
			out := map[string]string{}
			for _, f := range e.Data {
				out[f.Key] = f.Text()
			}
			return out
		}
	}
	return nil
}

// A server whose model was found is saved blank, with the tool format that
// follows it; a second one serving the same model gets its own name.
func TestAddServer_SavesWhatTheCheckFound(t *testing.T) {
	rec := eventtest.Capture(t)
	srv := (&fakeServer{models: []string{"Qwen/Qwen3.8-27B"}, owned: "llamacpp", chat: nativeAdd}).start(t)
	dir := t.TempDir()
	mc := &modelControl{cfgDir: dir}
	added, err := mc.addServer(context.Background(), serverRequest{Address: srv.URL + "/v1/"}, surfaceDashboard)
	if err != nil {
		t.Fatal(err)
	}
	if added.Name != "qwen3.8-27b" || added.Saved != "auto" || !added.Auto || added.Tools != toolsNative {
		t.Errorf("added %+v", added)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	pc := lc.Providers["qwen3.8-27b"]
	if pc == nil || pc.Kind != "llamacpp" || pc.Endpoint != srv.URL || pc.Model != "" || pc.ToolFmt != "auto" || pc.Auth != (authCfg{}) {
		t.Errorf("saved %+v", pc)
	}
	if lc.Embed == nil || lc.Provider != "" {
		t.Errorf("embed %+v, active %q: an added server is not switched to", lc.Embed, lc.Provider)
	}
	d := eventData(rec, "model", model.ServerAdded.Spec().ID)
	if d == nil || d["provider"] != "qwen3.8-27b" || d["kind"] != "llamacpp" || d["endpoint"] != srv.URL || d["model"] != "Qwen/Qwen3.8-27B" || d["tools"] != "native" || d["format"] != "qwen" {
		t.Errorf("ServerAdded = %v", d)
	}
	again, err := mc.addServer(context.Background(), serverRequest{Address: srv.URL}, surfaceDashboard)
	if err != nil || again.Name != "qwen3.8-27b-2" {
		t.Errorf("second add = %+v, %v", again, err)
	}
}

// A typed model is saved as typed (the server's spelling), with its format,
// and a key is stored under the model's name and used.
func TestAddServer_TypedModelAndKey(t *testing.T) {
	srv := (&fakeServer{models: []string{"Qwen3-32B", "gemma-3-27b-it"}, owned: "vllm", key: "sk-k", chat: nativeAdd}).start(t)
	dir := t.TempDir()
	mc := &modelControl{cfgDir: dir}
	added, err := mc.addServer(context.Background(), serverRequest{Address: srv.URL, Model: "GEMMA-3-27B-IT", Name: "office-gpu", APIKey: " sk-k "}, surfaceDashboard)
	if err != nil {
		t.Fatal(err)
	}
	lc, _ := loadLaunchConfig(dir)
	pc := lc.Providers["office-gpu"]
	if added.Name != "office-gpu" || pc == nil || pc.Kind != "vllm" || pc.Model != "gemma-3-27b-it" || pc.ToolFmt != "gemma" || !pc.Auth.Stored {
		t.Fatalf("added %+v, saved %+v", added, pc)
	}
	if got := resolveSecret(dir, "office-gpu", pc.Auth); got != "sk-k" {
		t.Errorf("stored key = %q", got)
	}
}

// Found automatically, but the test call wrote a format the model's name
// wouldn't pick: that format is kept, since the check saw it.
func TestAddServer_KeepsAFormatTheNameWouldMiss(t *testing.T) {
	srv := (&fakeServer{models: []string{"acme-coder-7b"}, owned: "vllm",
		chat: chatAnswer(map[string]any{"role": "assistant", "content": `<tool_call>{"name":"add","arguments":{"a":2,"b":3}}</tool_call>`})}).start(t)
	dir := t.TempDir()
	added, err := (&modelControl{cfgDir: dir}).addServer(context.Background(), serverRequest{Address: srv.URL}, surfaceDashboard)
	if err != nil {
		t.Fatal(err)
	}
	lc, _ := loadLaunchConfig(dir)
	if pc := lc.Providers[added.Name]; pc == nil || pc.Model != "" || pc.ToolFmt != "qwen" {
		t.Errorf("saved %+v", pc)
	}
}

// Names: a taken one, a catalog kind's, or one config.json can't key are
// refused before the server is asked; a found name steers clear of both.
func TestAddServer_Names(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "vllm", "owned_by": "vllm"}}})
		default:
			nativeAdd(w, nil)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := (&launchConfig{Providers: map[string]*providerConfig{"gpu": {Kind: "vllm", Endpoint: "http://127.0.0.1:1"}}}).Save(dir); err != nil {
		t.Fatal(err)
	}
	mc := &modelControl{cfgDir: dir}
	for name, want := range map[string]string{
		"gpu":       "You already have a model named gpu.",
		"Ollama":    "Ollama is the name of a kind of model Tenant knows.",
		"bad name!": "A name can have letters, numbers, dots, dashes and underscores",
		"-dash":     "A name can have letters, numbers, dots, dashes and underscores",
	} {
		_, err := mc.addServer(context.Background(), serverRequest{Address: srv.URL, Name: name}, surfaceDashboard)
		if p := plainOf(t, err); !strings.HasPrefix(p, want) {
			t.Errorf("name %q: %q, want %q…", name, p, want)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the server was asked %d time(s) before the name was refused", n)
	}
	added, err := mc.addServer(context.Background(), serverRequest{Address: srv.URL}, surfaceDashboard)
	if err != nil || added.Name != "vllm-2" {
		t.Errorf("a model served as vllm = %+v, %v; want the name vllm-2", added, err)
	}
}

func TestSuggestServerName(t *testing.T) {
	for _, c := range []struct{ model, endpoint, want string }{
		{"Qwen/Qwen3.8-27B", "http://h:1", "qwen3.8-27b"},
		{"hf.co/unsloth/gemma-3-27b-it-GGUF:Q4_K_M", "http://h:1", "gemma-3-27b-it-gguf-q4_k_m"},
		{"llama3.1:8b", "http://h:1", "llama3.1-8b"},
		{"???", "http://my-server:8081", "my-server-8081"},
		{"", "http://[::1]", "1"},
		{strings.Repeat("a", 60), "http://h:1", strings.Repeat("a", 32)},
	} {
		if got := suggestServerName(c.model, c.endpoint); got != c.want {
			t.Errorf("suggestServerName(%q, %q) = %q, want %q", c.model, c.endpoint, got, c.want)
		}
	}
}

// A server added without a key never inherits one left under its name.
func TestAddServer_KeylessDropsAStaleKey(t *testing.T) {
	srv := (&fakeServer{models: []string{"m"}, owned: "vllm", chat: nativeAdd}).start(t)
	dir := t.TempDir()
	creds, _ := loadCredentials(dir)
	creds.Set("gpu", "sk-old")
	if err := saveCredentials(dir, creds, surfaceCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := (&modelControl{cfgDir: dir}).addServer(context.Background(), serverRequest{Address: srv.URL, Name: "gpu"}, surfaceDashboard); err != nil {
		t.Fatal(err)
	}
	creds, _ = loadCredentials(dir)
	if _, ok := creds.Secrets["gpu"]; ok {
		t.Error("the old key is still stored under the new server's name")
	}
}

// A refused add is logged with what the page said and why; an address that
// isn't one (it may be a key pasted in the wrong box) is never logged.
func TestAddServer_FailuresAreLogged(t *testing.T) {
	rec := eventtest.Capture(t)
	srv := (&fakeServer{models: []string{"a", "b"}, owned: "vllm", chat: nativeAdd}).start(t)
	mc := &modelControl{cfgDir: t.TempDir()}
	if _, err := mc.addServer(context.Background(), serverRequest{Address: srv.URL}, surfaceDashboard); err == nil {
		t.Fatal("several models and none named: want an error")
	}
	d := eventData(rec, "model", model.ServerNotAdded.Spec().ID)
	if d == nil || d["endpoint"] != srv.URL || !strings.HasPrefix(d["problem"], "The server offers several models") {
		t.Errorf("ServerNotAdded = %v", d)
	}
	rec = eventtest.Capture(t)
	if _, err := mc.addServer(context.Background(), serverRequest{Address: "sk-live-SENTINEL-0123"}, surfaceDashboard); err == nil {
		t.Fatal("want an error")
	}
	for _, e := range rec.Events() {
		for _, f := range e.Data {
			if strings.Contains(f.Text(), "SENTINEL") {
				t.Errorf("%s %d logged the typed value in %s", e.Provider, e.ID, f.Key)
			}
		}
	}
}

// The Model page's rows: a self-hosted server shows its address (the kind's
// own when it was saved without one) and whether its model is found; a
// hosted one shows neither.
func TestDashModel_ServerRows(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Provider: "zai", Providers: map[string]*providerConfig{
		"gpu":   {Kind: "llamacpp", Endpoint: "http://my-server:8081", ToolFmt: "auto"},
		"box":   {Kind: "ollama", Model: "llama3.1:8b"},
		"zai":   {Kind: "zai", Endpoint: "https://api.z.ai/api/coding/paas/v4", Model: "glm-4.6"},
		"cc":    {Kind: "claudecode", Endpoint: "claudecode://auto", Model: "opus"},
		"plain": {Kind: "vllm", Endpoint: "http://other:8000", Model: "m"},
	}}
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range (dashModel{mc: &modelControl{cfgDir: dir}}).Models() {
		got[v.Name] = fmt.Sprintf("%s|%v", v.Address, v.AutoModel)
	}
	want := map[string]string{
		"gpu": "http://my-server:8081|true", "box": "http://localhost:11434|false",
		"zai": "|false", "cc": "|false", "plain": "http://other:8000|false",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: %q, want %q", name, got[name], w)
		}
	}
}

// tool_format "auto" becomes the served model's family's format, or the
// OpenAI pass-through for a family toolfmt doesn't read.
func TestVLLMGenProfiles_AutoToolFormat(t *testing.T) {
	for m, want := range map[string]string{"gemma-3-27b": "gemma", "Qwen3-32B": "qwen", "phi-4": "openai"} {
		for _, p := range vllmGenProfiles(genProfileOpts{endpoint: "http://h:1", model: m, toolFmt: "auto"}) {
			if p.ToolFormat != want {
				t.Errorf("%s: %s tool format = %q, want %q", m, p.ID, p.ToolFormat, want)
			}
		}
	}
}

// servedModels answers /v1/models with one model and its context window.
func servedModels(t *testing.T, id string, maxLen int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": id, "owned_by": "vllm", "max_model_len": maxLen}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A server whose model is found works as a fallback or planner-big model,
// and as a helper's: each asks the server, as the main model's start does.
func TestAutoModel_WorksBeyondTheMainModel(t *testing.T) {
	srv := servedModels(t, "Qwen3-32B", 32768)
	lc := &launchConfig{Providers: map[string]*providerConfig{"gpu": {Kind: "llamacpp", Endpoint: srv.URL, ToolFmt: "auto"}}}
	gen, _, err := providerGenProfiles(t.TempDir(), lc, "gpu", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range gen {
		if p.Model != "Qwen3-32B" || p.ContextLength != 32768 || p.ToolFormat != "qwen" || !strings.HasPrefix(p.ID, "gpu/") {
			t.Errorf("fallback profile %s: model %q, context %d, format %q", p.ID, p.Model, p.ContextLength, p.ToolFormat)
		}
	}
	r, err := buildProfileRouter("coder", &agentProfile{Provider: "gpu"}, lc, t.TempDir(), model.Profile{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.ForRole(model.RolePlanner)
	if err != nil || p.Model != "Qwen3-32B" || p.ContextLength != 32768 || p.ToolFormat != "qwen" {
		t.Errorf("helper planner = %+v, %v", p, err)
	}
	// A server that can't be asked keeps the error it always gave.
	srv.Close()
	if _, _, err := providerGenProfiles(t.TempDir(), lc, "gpu", 0); err == nil {
		t.Error("an unreachable server with no model: want an error")
	}
}

// TEN-507: a named model still gets its server-reported context window, not
// the 128K default, at launch / /model use, as a fallback and as a helper's.
func TestNamedModel_UsesServedContextLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "Qwen3-32B", "max_model_len": 32768},
			map[string]any{"id": "glm-5.3-flash", "max_model_len": 500000},
		}})
	}))
	t.Cleanup(srv.Close)

	c := &commonFlags{backend: "vllm", genKind: "llamacpp", vllmEndpoint: srv.URL, vllmModel: "glm-5.3-flash"}
	c.autodetectModel()
	if c.vllmModel != "glm-5.3-flash" || c.contextLen != 500000 {
		t.Errorf("launch: model %q, context %d; want glm-5.3-flash, 500000", c.vllmModel, c.contextLen)
	}

	lc := &launchConfig{Providers: map[string]*providerConfig{"gpu": {Kind: "llamacpp", Endpoint: srv.URL, Model: "glm-5.3-flash", ToolFmt: "glm"}}}
	gen, _, err := providerGenProfiles(t.TempDir(), lc, "gpu", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range gen {
		if p.Model != "glm-5.3-flash" || p.ContextLength != 500000 {
			t.Errorf("fallback profile %s: model %q, context %d", p.ID, p.Model, p.ContextLength)
		}
	}
	r, err := buildProfileRouter("coder", &agentProfile{Provider: "gpu"}, lc, t.TempDir(), model.Profile{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := r.ForRole(model.RolePlanner); err != nil || p.ContextLength != 500000 {
		t.Errorf("helper planner = %+v, %v", p, err)
	}

	// A model the server doesn't list keeps the default.
	c = &commonFlags{backend: "vllm", genKind: "llamacpp", vllmEndpoint: srv.URL, vllmModel: "gone"}
	c.autodetectModel()
	if c.vllmModel != "gone" || c.contextLen != 0 {
		t.Errorf("unlisted model: model %q, context %d; want gone, 0", c.vllmModel, c.contextLen)
	}
}
