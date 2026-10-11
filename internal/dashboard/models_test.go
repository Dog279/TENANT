package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

type fakeModelCtl struct {
	judgeOn     bool
	judgeKey    string
	judgeSets   []string
	models      []ModelView
	ceiling     int
	useCalls    []string
	addCalls    []string // "kind|key"
	serverCalls []string // "address|model|name|key"
	server      ServerAdded
	serverErr   error
	removeCalls []string
	ceilingSet  int
	repeatSet   []int
	reloadCalls int
}

func (f *fakeModelCtl) Models() []ModelView { return f.models }
func (f *fakeModelCtl) Use(name string) (string, error) {
	f.useCalls = append(f.useCalls, name)
	return "switched to " + name, nil
}
func (f *fakeModelCtl) AddCloud(kind, key string) (string, error) {
	f.addCalls = append(f.addCalls, kind+"|"+key)
	return "added " + kind, nil
}

// AddServer answers f.server, by default a llama.cpp server whose model was
// found and whose tool calls came back the standard way.
func (f *fakeModelCtl) AddServer(_ context.Context, req ServerRequest) (ServerAdded, error) {
	f.serverCalls = append(f.serverCalls, req.Address+"|"+req.Model+"|"+req.Name+"|"+req.APIKey)
	if f.serverErr != nil {
		return ServerAdded{}, f.serverErr
	}
	if f.server.Name == "" {
		return ServerAdded{Name: "gpu", Server: "a llama.cpp server", Model: "qwen3.8-27b", Auto: true, Tools: "native", Format: "Qwen (Hermes)"}, nil
	}
	return f.server, nil
}
func (f *fakeModelCtl) Remove(name string) (string, error) {
	f.removeCalls = append(f.removeCalls, name)
	return "removed " + name, nil
}
func (f *fakeModelCtl) LoopCeiling() int { return f.ceiling }
func (f *fakeModelCtl) SetLoopCeiling(n int) (string, error) {
	f.ceilingSet = n
	return "ceiling set", nil
}
func (f *fakeModelCtl) RepeatGuard() int { return 8 }
func (f *fakeModelCtl) Vision() string   { return "vision: auto (probe: yes), glm can see images" }
func (f *fakeModelCtl) Judge() JudgeView {
	return JudgeView{On: f.judgeOn, Kind: "clef", Kinds: []string{"clef"}, Endpoint: "http://judge:30000", Model: "clef-flash", Rounds: 3, HasKey: f.judgeKey != ""}
}
func (f *fakeModelCtl) SetJudge(on bool, kind, endpoint, key, model string, rounds int) (string, error) {
	f.judgeOn = on
	if key != "" {
		f.judgeKey = key
	}
	f.judgeSets = append(f.judgeSets, fmt.Sprintf("%v|%s|%s|%d|%s|%d", on, kind, endpoint, len(key), model, rounds))
	return "judge: " + map[bool]string{true: "on", false: "off"}[on], nil
}
func (f *fakeModelCtl) SetRepeatGuard(n int) (string, error) {
	f.repeatSet = append(f.repeatSet, n)
	return "repeat set", nil
}
func (f *fakeModelCtl) ReloadKeys() (string, error) { f.reloadCalls++; return "keys reloaded", nil }

func modelServer(fc ModelControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetModels(fc)
	}
	return s
}

func TestModels_PageNotConfigured(t *testing.T) {
	rec := get(t, modelServer(nil), "/models")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Model settings aren&#39;t available on this Tenant.") {
		t.Errorf("nil models should render not-configured; got %d:\n%s", rec.Code, rec.Body.String())
	}
}

func TestModels_ListsBackendsAndMarksActive(t *testing.T) {
	fc := &fakeModelCtl{ceiling: 30, models: []ModelView{
		{Name: "zai", Kind: "openai", Model: "glm-4.6", Active: true},
		{Name: "local", Kind: "vllm", Model: "qwen", Active: false},
	}}
	body := get(t, modelServer(fc), "/models").Body.String()
	for _, want := range []string{"zai", "glm-4.6", "active", "local", "qwen", "Add a cloud model", "30"} {
		if !strings.Contains(body, want) {
			t.Errorf("models page missing %q:\n%s", want, body)
		}
	}
	// the active model must NOT offer a "Use this model" button or a Remove
	const use = `<span class="c-open">Use this model</span>` // its confirm step's first button
	if strings.Count(body, use) != 1 {
		t.Errorf("exactly the one inactive model should offer Use; got %d", strings.Count(body, use))
	}
}

func TestModels_SwitchRequiresConfirm(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	// Without confirm=yes, the swap must NOT happen.
	submitForm(t, s, "/models/use", "name=local")
	if len(fc.useCalls) != 0 {
		t.Errorf("switch without confirm should not call Use; got %v", fc.useCalls)
	}
	// With confirm=yes it goes through.
	submitForm(t, s, "/models/use", "name=local&confirm=yes")
	if len(fc.useCalls) != 1 || fc.useCalls[0] != "local" {
		t.Errorf("confirmed switch calls = %v", fc.useCalls)
	}
}

func TestModels_AddRemoveCeilingReload(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	submitForm(t, s, "/models/add", "kind=anthropic&key=sk-test")
	if len(fc.addCalls) != 1 || fc.addCalls[0] != "anthropic|sk-test" {
		t.Errorf("add calls = %v", fc.addCalls)
	}
	submitForm(t, s, "/models/remove", "name=old&confirm=yes")
	if len(fc.removeCalls) != 1 || fc.removeCalls[0] != "old" {
		t.Errorf("remove calls = %v", fc.removeCalls)
	}
	submitForm(t, s, "/models/ceiling", "ceiling=42")
	if fc.ceilingSet != 42 {
		t.Errorf("ceiling set = %d, want 42", fc.ceilingSet)
	}
	submitForm(t, s, "/models/reload", "")
	if fc.reloadCalls != 1 {
		t.Errorf("reload calls = %d, want 1", fc.reloadCalls)
	}
}

func TestModels_AddRejectsEmpty(t *testing.T) {
	fc := &fakeModelCtl{}
	submitForm(t, modelServer(fc), "/models/add", "kind=&key=")
	if len(fc.addCalls) != 0 {
		t.Errorf("empty add should be rejected; got %v", fc.addCalls)
	}
}

func TestModels_CeilingRejectsNonPositive(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	submitForm(t, s, "/models/ceiling", "ceiling=0")
	submitForm(t, s, "/models/ceiling", "ceiling=abc")
	if fc.ceilingSet != 0 {
		t.Errorf("invalid ceiling should be rejected; got %d", fc.ceilingSet)
	}
}

// TEN-506: the repeat guard is settable live; 0 in the form means off (-1),
// and 1 or junk is refused.
func TestModels_RepeatGuard(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	for _, v := range []string{"12", "0", "1", "abc", "-3"} {
		submitForm(t, s, "/models/repeat", "repeat="+v)
	}
	if !slices.Equal(fc.repeatSet, []int{12, -1}) {
		t.Errorf("repeat set = %v, want [12 -1]", fc.repeatSet)
	}
}

// TEN-510, TEN-516: the Judge card shows the saved kind, server, model and
// rounds, says whether a key is saved without showing it, and the form
// switches the judge on and off, passing the kind, key and rounds through.
// The key never comes back in the page or a message.
func TestModels_Judge(t *testing.T) {
	fc := &fakeModelCtl{}
	s := modelServer(fc)
	body := get(t, s, "/models").Body.String()
	for _, want := range []string{`action="/models/judge"`, `name="kind"`, `<option value="clef" selected>`, `value="http://judge:30000"`, `value="clef-flash"`,
		`type="password" name="key"`, `only if the server asks for one`, `name="rounds" value="3"`, "checks the agent's answers to tasks", `input[type=password]`} {
		if !strings.Contains(body, want) {
			t.Errorf("models page lacks %q", want)
		}
	}
	submitForm(t, s, "/models/judge", "judge=on&kind=clef&endpoint=http://judge:30000&key=sek-77&model=clef-flash&rounds=2")
	if !fc.judgeOn || len(fc.judgeSets) != 1 || fc.judgeSets[0] != "true|clef|http://judge:30000|6|clef-flash|2" {
		t.Fatalf("judge=on: on=%v sets=%v", fc.judgeOn, fc.judgeSets)
	}
	body = get(t, s, "/models").Body.String()
	if strings.Contains(body, "sek-77") || !strings.Contains(body, "a key is saved; blank keeps it") {
		t.Errorf("the key must not come back, only the fact that one is saved:\n%s", body)
	}
	rec := submitForm(t, s, "/models/judge", "judge=off")
	if fc.judgeOn || strings.Contains(rec.Body.String(), "sek-77") {
		t.Fatal("judge=off didn't switch it off, or the key leaked")
	}
}

func TestModels_MutationsNilSafe(t *testing.T) {
	s := modelServer(nil)
	for _, p := range []string{"/models/use", "/models/add", "/models/add-server", "/models/remove", "/models/ceiling", "/models/repeat", "/models/judge", "/models/reload"} {
		if rec := submitForm(t, s, p, "name=x&kind=zai&key=k&address=http://h:1&ceiling=5&confirm=yes"); rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control: want 303, got %d", p, rec.Code)
		}
	}
}

// The server form: an address (required), a model, and behind More options a
// name and a write-only key; it says it's checking while its answer is out.
func TestModels_ServerFormRenders(t *testing.T) {
	body := get(t, modelServer(&fakeModelCtl{}), "/models").Body.String()
	for _, want := range []string{
		"Add a model from a server on your network",
		`<input type="url" name="address" class="cinput wide" required`,
		`name="model"`, `name="name"`,
		`<input type="password" name="key" class="cinput wide" autocomplete="off"`,
		`<details class="advanced"><summary>More options</summary>`,
		`data-indicator:_checking`, `data-attr:disabled="$_checking"`,
		`<span class="loading" style="display:none" data-show="$_checking">Checking the server</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the models page is missing %q", want)
		}
	}
}

// A server's row says where it is, and that its model is found from it.
func TestModels_ServerRows(t *testing.T) {
	fc := &fakeModelCtl{models: []ModelView{
		{Name: "gpu", Kind: "llamacpp", Address: "http://my-server:8081", AutoModel: true},
		{Name: "pinned", Kind: "vllm", Model: "qwen", Address: "http://other:8000"},
		{Name: "zai", Kind: "zai", Model: "glm-4.6"},
	}}
	body := get(t, modelServer(fc), "/models").Body.String()
	for _, want := range []string{
		`<div class="dim">llamacpp · model found automatically</div>`, `<div class="dim">http://my-server:8081</div>`,
		`<div class="dim">vllm · qwen</div>`, `<div class="dim">http://other:8000</div>`,
		`<div class="dim">zai · glm-4.6</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the rows are missing %q", want)
		}
	}
}

func TestModels_AddServerPassesTheForm(t *testing.T) {
	fc := &fakeModelCtl{}
	submitForm(t, modelServer(fc), "/models/add-server", "address=+http://my-server:8080+&model=+qwen+&name=+gpu+&key=+sk-x+")
	if len(fc.serverCalls) != 1 || fc.serverCalls[0] != "http://my-server:8080|qwen|gpu|sk-x" {
		t.Errorf("server calls = %q", fc.serverCalls)
	}
	fc = &fakeModelCtl{}
	rec := submitForm(t, modelServer(fc), "/models/add-server", "address=+&model=qwen")
	if loc, _ := url.QueryUnescape(rec.Header().Get("Location")); len(fc.serverCalls) != 0 || !strings.Contains(loc, "Enter the server's address") {
		t.Errorf("no address: calls %q, answer %q", fc.serverCalls, loc)
	}
}

// What the add found decides the answer: a server whose tool calls came back
// is a success; one that didn't use the tool, or wasn't checked in time, is
// an alert. Either way the model was added, so the page's part comes back
// whole with the form emptied.
func TestModels_AddServerAnswers(t *testing.T) {
	for _, c := range []struct {
		added ServerAdded
		toast string
	}{
		{ServerAdded{Name: "gpu", Server: "a llama.cpp server", Model: "qwen3.8-27b", Auto: true, Tools: "native"},
			`<div id="toast" class="toast ok"><span class="toast-text">Added gpu: a llama.cpp server running qwen3.8-27b, found automatically. It returns tool calls the standard way. Choose Use this model`},
		{ServerAdded{Name: "box", Server: "an OpenAI-compatible server", Model: "m", Tools: "text", Format: "Mistral"},
			`<div id="toast" class="toast ok"><span class="toast-text">Added box: an OpenAI-compatible server running m. It writes tool calls as text, in the Mistral format, which your agent reads.`},
		{ServerAdded{Name: "old", Server: "Ollama", Model: "m", Tools: "none"},
			`<div id="toast" class="toast err" role="alert"><span class="toast-text">Added old: Ollama running m. But it answered a test message without using the tool it was offered`},
		{ServerAdded{Name: "cold", Server: "a vLLM server", Model: "m", Tools: "unchecked", Format: "Qwen (Hermes)"},
			`<div id="toast" class="toast err" role="alert"><span class="toast-text">Added cold: a vLLM server running m. Its test message didn&#39;t finish in time, so its tool calls weren&#39;t checked. Your agent will read them in the Qwen (Hermes) format.`},
	} {
		fc := &fakeModelCtl{server: c.added}
		form := url.Values{"address": {"http://my-server:8080"}}
		body := sendForm(modelServer(fc).Handler(), http.MethodPost, "/models/add-server", form, datastar).Body.String()
		if !strings.Contains(body, c.toast) {
			t.Errorf("%s: want the toast %q:\n%s", c.added.Name, c.toast, body)
		}
		if !strings.Contains(body, "data: mode replace\ndata: elements <div id=\"models-body\">") {
			t.Errorf("%s: the page's part didn't come back whole:\n%s", c.added.Name, body)
		}
		rec := sendForm(modelServer(fc).Handler(), http.MethodPost, "/models/add-server", form, nil)
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/models?") {
			t.Errorf("%s: a plain post = %d to %q", c.added.Name, rec.Code, loc)
		}
	}
}

// A failure the control phrases is shown as it is; any other is a fixed
// message. Neither re-renders the page, so what was typed stays.
func TestModels_AddServerFailures(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{PlainError("The server offers several models: a, b. Type the one you want in Model."), "The server offers several models: a, b. Type the one you want in Model."},
		{errors.New("dial tcp: connection refused"), "Couldn&#39;t add that server. The Logs page has the reason."},
	} {
		fc := &fakeModelCtl{serverErr: c.err}
		body := sendForm(modelServer(fc).Handler(), http.MethodPost, "/models/add-server", url.Values{"address": {"http://my-server:8080"}}, datastar).Body.String()
		if !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">`+c.want) {
			t.Errorf("want the alert %q:\n%s", c.want, body)
		}
		if strings.Contains(body, `id="models-body"`) || strings.Contains(body, "connection refused") {
			t.Errorf("a failure re-rendered the page or said too much:\n%s", body)
		}
	}
}

func TestHome_ShowsActiveModel(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetModels(&fakeModelCtl{models: []ModelView{{Name: "zai", Model: "glm-4.6", Active: true}}})
	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "Model") || !strings.Contains(body, "zai · glm-4.6") {
		t.Errorf("home should show the active model:\n%s", body)
	}
}
