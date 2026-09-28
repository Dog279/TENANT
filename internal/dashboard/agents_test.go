package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// fakeAgents records every change as "Method|arg|arg…" and fails reads or
// changes on demand.
type fakeAgents struct {
	rows    []AgentProfileView
	souls   map[string]string
	loadErr error // Agents and Soul
	err     error // every change
	restart string
	calls   []string
}

func (f *fakeAgents) Agents() ([]AgentProfileView, error) { return f.rows, f.loadErr }
func (f *fakeAgents) Soul(name string) (string, error) {
	f.calls = append(f.calls, "Soul|"+name)
	return f.souls[name], f.loadErr
}
func (f *fakeAgents) record(parts ...string) error {
	f.calls = append(f.calls, strings.Join(parts, "|"))
	return f.err
}
func (f *fakeAgents) Add(n, p, m, d string) error   { return f.record("Add", n, p, m, d) }
func (f *fakeAgents) SetModel(n, p, m string) error { return f.record("SetModel", n, p, m) }
func (f *fakeAgents) SetSoul(n, s string) error     { return f.record("SetSoul", n, s) }
func (f *fakeAgents) Rename(from, to string) error  { return f.record("Rename", from, to) }
func (f *fakeAgents) Remove(n string) error         { return f.record("Remove", n) }
func (f *fakeAgents) RestartNote() string           { return f.restart }
func (f *fakeAgents) SendsTo(kind string) string {
	if kind == "openai" {
		return "OpenAI"
	}
	return ""
}
func (f *fakeAgents) changes() []string {
	return slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return strings.HasPrefix(c, "Soul|") })
}

// sampleAgents is one helper of each kind: a built-in, a customized
// built-in, one of the operator's own on a hosted provider, and a broken one.
func sampleAgents() *fakeAgents {
	return &fakeAgents{
		rows: []AgentProfileView{
			{Name: "Programmer", Description: "implements features", HasSoul: true, Builtin: true, Valid: true},
			{Name: "QA", Description: "tries to break the work", HasSoul: true, Customized: true, Valid: true},
			{Name: "researcher", Provider: "cloud", Model: "gpt-4o", SendsTo: "OpenAI", Description: "reads the web", Valid: true},
			{Name: "orphan", Provider: "gone", Valid: false},
		},
		souls: map[string]string{"Programmer": "Smallest diff first.", "researcher": ""},
	}
}

func agentsServer(fa AgentControl, models ...ModelView) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fa != nil {
		s.SetAgents(fa)
	}
	if models != nil {
		s.SetModels(&fakeModelCtl{models: models})
	}
	return s
}

var cloudModels = []ModelView{{Name: "cloud", Kind: "openai", Model: "gpt-4o"}, {Name: "gpu", Kind: "vllm", Model: "aeon"}}

// The harness fakes: the 390px worst case, and every read failing with the
// raw text agentControl gives (agentctl.go:91), which must never show.
func init() {
	harnessFakes = append(harnessFakes, func(s *Server, mode fakeMode) {
		fa := &fakeAgents{souls: map[string]string{}}
		for i := range 14 {
			v := AgentProfileView{Name: longName(i), Description: longURL(i), HasSoul: true, Valid: i != 3}
			switch i % 3 {
			case 0:
				v.Builtin = true
			case 1:
				v.Customized = true
			default:
				v.Provider, v.Model, v.SendsTo = longName(i+30), longName(i+60), "OpenAI"
			}
			fa.rows = append(fa.rows, v)
			fa.souls[v.Name] = longDetail()
		}
		if mode == failing {
			fa.loadErr = errors.New(rawProviderErr)
		}
		s.SetAgents(fa)
	})
	harnessPages = append(harnessPages, "/agents?edit="+longName(2))
}

// The states the page can be in (DESIGN.md §6). It has no loading state: it
// renders complete. Busy is the status strip every page carries.
func TestAgents_States(t *testing.T) {
	failing := sampleAgents()
	failing.loadErr = errors.New(rawProviderErr)
	for _, tc := range []struct {
		name, path string
		ctl        AgentControl
		want       []string
		not        []string
	}{
		{"off", "/agents", nil, []string{`class="card state-off"`, "Helpers aren&#39;t available on this Tenant."}, []string{"Add a helper"}},
		{"empty", "/agents", &fakeAgents{}, []string{`<div class="empty">No helpers yet.`}, nil},
		{"error", "/agents?err=Nope.", sampleAgents(), []string{`<div class="banner err" role="alert">Nope.</div>`}, nil},
		{"read failed", "/agents", failing, []string{`role="alert">Couldn&#39;t read your helpers.`}, []string{"not configured", "No helpers yet"}},
		{"success", "/agents?msg=Saved.", sampleAgents(), []string{`<div class="banner ok" role="status">Saved.</div>`}, nil},
		{"busy", "/agents", sampleAgents(), []string{`class="status-wrap"`, `id="ui-status"`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, agentsServer(tc.ctl, cloudModels...), tc.path)
			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d", tc.path, rec.Code)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("lacks %q", w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(body, n) {
					t.Errorf("shows %q", n)
				}
			}
		})
	}
}

func TestAgents_List(t *testing.T) {
	body := get(t, agentsServer(sampleAgents(), cloudModels...), "/agents").Body.String()
	for _, want := range []string{
		`<span class="tag">Built in</span>`, `<span class="tag">Customized</span>`,
		"Uses your main model", "cloud · gpt-4o", "Sends its work to OpenAI.",
		"Needs attention: its model provider is missing",
		`href="/agents?edit=researcher#edit"`,
		`action="/agents/reset"`, "Undo your changes to QA?",
		`action="/agents/remove"`, "Remove researcher?",
		`action="/agents/add"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
	row := func(name string) string {
		i := strings.Index(body, `id="agent-`+name+`"`)
		j := strings.Index(body[i+1:], `class="row agent-row"`)
		if j < 0 {
			j = len(body) - i - 1
		}
		return body[i : i+1+j]
	}
	if prog := row("Programmer"); strings.Contains(prog, "/agents/remove") || strings.Contains(prog, "/agents/reset") || strings.Contains(prog, "rename") {
		t.Errorf("a built-in offers Remove, Reset or Rename:\n%s", prog)
	}
	if qa := row("QA"); strings.Contains(qa, "/agents/remove") {
		t.Errorf("a customized built-in offers Remove, not just Reset:\n%s", qa)
	}
	if strings.Contains(body, `id="edit"`) {
		t.Error("the edit panel shows without ?edit=")
	}
}

// ?edit= opens the helper's panel: model, name (only for the operator's own
// helpers), and the instructions editor behind Advanced (D32), with Clear as
// a confirm step.
func TestAgents_EditPanelAdvanced(t *testing.T) {
	fa := sampleAgents()
	// A Windows checkout gives the templates CRLF line ends.
	body := strings.ReplaceAll(get(t, agentsServer(fa, cloudModels...), "/agents?edit=Programmer").Body.String(), "\r\n", "\n")
	if !slices.Contains(fa.calls, "Soul|Programmer") {
		t.Errorf("Soul not read for the editor: %q", fa.calls)
	}
	adv := body[strings.Index(body, `<details class="advanced"><summary>Advanced</summary>`):]
	adv = adv[:strings.Index(adv, "</details>\n")]
	for _, want := range []string{`<textarea name="soul"`, "Smallest diff first.</textarea>", `action="/agents/soul"`, `class="confirm-step"`,
		"Clear this helper&#39;s instructions? Its current instructions are deleted and can&#39;t be brought back here."} {
		if !strings.Contains(adv, want) {
			t.Errorf("Advanced lacks %q:\n%s", want, adv)
		}
	}
	if strings.Contains(body, `action="/agents/rename"`) {
		t.Error("a built-in's panel offers Rename")
	}
	// A hosted provider says where the work goes before it's picked (D18).
	if !strings.Contains(body, `<option value="cloud">cloud · gpt-4o · sends its work to OpenAI</option>`) || !strings.Contains(body, `<option value="gpu">gpu · aeon</option>`) {
		t.Error("the model choices are missing, or don't say which send work away")
	}

	body = get(t, agentsServer(sampleAgents(), cloudModels...), "/agents?edit=researcher").Body.String()
	if !strings.Contains(body, `action="/agents/rename"`) || !strings.Contains(body, `<option value="cloud" selected>`) {
		t.Error("the operator's own helper lacks Rename, or its provider isn't selected")
	}
	if !strings.Contains(body, "Now: cloud · gpt-4o. Sends its work to OpenAI.") {
		t.Error("the current model doesn't say where the work goes")
	}
	if strings.Contains(body, "Clear instructions") {
		t.Error("Clear is offered for a helper with no instructions")
	}
	if strings.Contains(get(t, agentsServer(sampleAgents(), cloudModels...), "/agents?edit=nobody").Body.String(), `id="edit"`) {
		t.Error("a panel opened for a helper that doesn't exist")
	}
}

// Without a model the page points to the Model page instead of the forms.
func TestAgents_NoModels(t *testing.T) {
	for _, s := range []*Server{agentsServer(sampleAgents()), agentsServer(sampleAgents(), []ModelView{}...)} {
		body := get(t, s, "/agents?edit=researcher").Body.String()
		if strings.Contains(body, `action="/agents/add"`) || strings.Contains(body, `action="/agents/model"`) {
			t.Error("the add or model form shows without any model")
		}
		if !strings.Contains(body, `<a href="/models">Model page</a>`) {
			t.Error("no link to the Model page")
		}
	}
}

func post(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return submitForm(t, s, path, form.Encode())
}

func agentsFlash(t *testing.T, rec *httptest.ResponseRecorder) (page, msg, errMsg string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u.Path + "?" + u.Query().Get("edit"), u.Query().Get("msg"), u.Query().Get("err")
}

// Remove, Reset and clearing the instructions need the confirm step (428
// without it, and the control is never called); saving instructions doesn't.
func TestAgents_ConfirmGates(t *testing.T) {
	for _, tc := range []struct {
		path string
		form url.Values
		call string
	}{
		{"/agents/remove", url.Values{"name": {"researcher"}}, "Remove|researcher"},
		{"/agents/reset", url.Values{"name": {"QA"}}, "Remove|QA"},
		{"/agents/soul", url.Values{"name": {"QA"}}, "SetSoul|QA|"},
		{"/agents/soul", url.Values{"name": {"QA"}, "soul": {" \n "}}, "SetSoul|QA| \n "},
	} {
		fa := sampleAgents()
		s := agentsServer(fa, cloudModels...)
		if rec := post(t, s, tc.path, tc.form); rec.Code != http.StatusPreconditionRequired || len(fa.changes()) != 0 {
			t.Errorf("%s %v without confirm: %d, calls %q; want 428 and none", tc.path, tc.form, rec.Code, fa.changes())
		}
		tc.form.Set("confirm", "yes")
		post(t, s, tc.path, tc.form)
		if !slices.Equal(fa.changes(), []string{tc.call}) {
			t.Errorf("%s confirmed: calls %q, want %q", tc.path, fa.changes(), tc.call)
		}
	}
	fa := sampleAgents()
	_, msg, _ := agentsFlash(t, post(t, agentsServer(fa, cloudModels...), "/agents/soul", url.Values{"name": {"QA"}, "soul": {"Be thorough."}}))
	if !slices.Equal(fa.changes(), []string{"SetSoul|QA|Be thorough."}) || msg != "Saved QA's instructions." {
		t.Errorf("saving instructions: calls %q, msg %q", fa.changes(), msg)
	}
}

func TestAgents_Changes(t *testing.T) {
	for _, tc := range []struct {
		path      string
		form      url.Values
		call      string
		page, msg string
	}{
		{"/agents/add", url.Values{"name": {" helper1 "}, "provider": {" cloud "}, "model": {" gpt-4o "}, "description": {" checks things "}},
			"Add|helper1|cloud|gpt-4o|checks things", "/agents?", "Added helper1. Your main agent learns about new and renamed helpers when Tenant restarts; until then, ask for helper1 by name in chat."},
		{"/agents/model", url.Values{"name": {"QA"}, "provider": {"cloud"}, "model": {" gpt-4o "}},
			"SetModel|QA|cloud|gpt-4o", "/agents?QA", "QA will use cloud · gpt-4o from its next task."},
		{"/agents/model", url.Values{"name": {"QA"}, "provider": {"gpu"}},
			"SetModel|QA|gpu|", "/agents?QA", "QA will use gpu from its next task."},
		{"/agents/rename", url.Values{"name": {"researcher"}, "new_name": {" reader "}},
			"Rename|researcher|reader", "/agents?", "Renamed researcher to reader. Your main agent learns about new and renamed helpers when Tenant restarts; until then, ask for reader by name in chat."},
		{"/agents/remove", url.Values{"name": {"researcher"}, "confirm": {"yes"}}, "Remove|researcher", "/agents?", "Removed researcher."},
		{"/agents/reset", url.Values{"name": {"QA"}, "confirm": {"yes"}}, "Remove|QA", "/agents?", "QA is back to its built-in model and instructions."},
		{"/agents/soul", url.Values{"name": {"QA"}, "confirm": {"yes"}}, "SetSoul|QA|", "/agents?QA", "Cleared QA's instructions."},
	} {
		fa := sampleAgents()
		page, msg, errMsg := agentsFlash(t, post(t, agentsServer(fa, cloudModels...), tc.path, tc.form))
		if !slices.Equal(fa.changes(), []string{tc.call}) || page != tc.page || msg != tc.msg || errMsg != "" {
			t.Errorf("%s: calls %q, back to %q, msg %q, err %q\nwant %q, %q, %q", tc.path, fa.changes(), page, msg, errMsg, tc.call, tc.page, tc.msg)
		}
	}
	// A Windows service says how to restart it.
	fa := sampleAgents()
	fa.restart = "Restart the Tenant service (open Services, find Tenant, click Restart)."
	if _, msg, _ := agentsFlash(t, post(t, agentsServer(fa, cloudModels...), "/agents/add", url.Values{"name": {"h"}, "provider": {"cloud"}})); !strings.HasSuffix(msg, "by name in chat. "+fa.restart) {
		t.Errorf("add on a service: %q", msg)
	}
}

// The page's own checks refuse before the control is called.
func TestAgents_Validation(t *testing.T) {
	for _, tc := range []struct {
		path string
		form url.Values
		err  string
	}{
		{"/agents/add", url.Values{"name": {"h"}}, "Pick a model provider for the helper."},
		{"/agents/add", url.Values{"name": {strings.Repeat("n", 65)}, "provider": {"cloud"}}, "Keep the name to 64 characters or fewer."},
		{"/agents/add", url.Values{"name": {"h"}, "provider": {"cloud"}, "description": {strings.Repeat("é", 501)}}, "Keep what it's for to 500 characters or fewer."},
		{"/agents/model", url.Values{"name": {"QA"}}, "Pick a model provider for the helper."},
		{"/agents/rename", url.Values{"name": {"researcher"}, "new_name": {"  "}}, "Type the helper's new name."},
		{"/agents/rename", url.Values{"name": {"researcher"}, "new_name": {strings.Repeat("n", 65)}}, "Keep the name to 64 characters or fewer."},
		{"/agents/add", url.Values{"name": {"h"}, "provider": {"cloud"}, "model": {strings.Repeat("m", 201)}}, "Keep the model name to 200 characters or fewer."},
		{"/agents/model", url.Values{"name": {"QA"}, "provider": {"cloud"}, "model": {strings.Repeat("m", 201)}}, "Keep the model name to 200 characters or fewer."},
		// Reset and Remove both end in the control's Remove: each acts only
		// on its own kind of helper.
		{"/agents/reset", url.Values{"name": {"researcher"}, "confirm": {"yes"}}, "Only a changed built-in helper can be reset."},
		{"/agents/reset", url.Values{"name": {"Programmer"}, "confirm": {"yes"}}, "Only a changed built-in helper can be reset."},
		{"/agents/remove", url.Values{"name": {"QA"}, "confirm": {"yes"}}, "Built-in helpers can't be removed. Reset undoes your changes to one."},
	} {
		fa := sampleAgents()
		_, _, errMsg := agentsFlash(t, post(t, agentsServer(fa, cloudModels...), tc.path, tc.form))
		if errMsg != tc.err || len(fa.changes()) != 0 {
			t.Errorf("%s %v: err %q, calls %q; want %q and none", tc.path, tc.form, errMsg, fa.changes(), tc.err)
		}
	}
	// No name, or no control: straight back to the page.
	for _, s := range []*Server{agentsServer(sampleAgents()), agentsServer(nil)} {
		for _, p := range []string{"/agents/add", "/agents/model", "/agents/soul", "/agents/rename", "/agents/remove", "/agents/reset"} {
			rec := post(t, s, p, url.Values{"name": {""}, "confirm": {"yes"}, "provider": {"cloud"}, "new_name": {"x"}, "soul": {"x"}})
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/agents" {
				t.Errorf("%s with no name or control = %d → %q", p, rec.Code, rec.Header().Get("Location"))
			}
		}
	}
	rec := post(t, agentsServer(nil), "/agents/remove", url.Values{"name": {"x"}, "confirm": {"yes"}})
	if rec.Header().Get("Location") != "/agents" {
		t.Errorf("remove with a nil control → %q", rec.Header().Get("Location"))
	}
}

// A control's PlainError shows as it is; any other error shows the action's
// own plain message and never its raw text (D21).
func TestAgents_ControlErrors(t *testing.T) {
	fa := sampleAgents()
	fa.err = PlainError("A helper named Programmer already exists.")
	if _, _, e := agentsFlash(t, post(t, agentsServer(fa, cloudModels...), "/agents/add", url.Values{"name": {"programmer"}, "provider": {"cloud"}})); e != string(fa.err.(PlainError)) {
		t.Errorf("plain error shown as %q", e)
	}
	fa.err = errors.New(rawProviderErr)
	for path, want := range map[string]string{
		"/agents/add":    "Couldn't add that helper.",
		"/agents/model":  "Couldn't change that helper's model.",
		"/agents/soul":   "Couldn't save the instructions.",
		"/agents/rename": "Couldn't rename that helper.",
		"/agents/remove": "Couldn't remove that helper.",
		"/agents/reset":  "Couldn't reset that helper.",
	} {
		name := "researcher"
		if path == "/agents/reset" {
			name = "QA"
		}
		rec := post(t, agentsServer(fa, cloudModels...), path, url.Values{"name": {name}, "provider": {"cloud"}, "new_name": {"reader"}, "soul": {"x"}, "confirm": {"yes"}})
		if _, _, e := agentsFlash(t, rec); e != want || strings.Contains(rec.Header().Get("Location"), "configured") {
			t.Errorf("%s: err %q (Location %q), want %q", path, e, rec.Header().Get("Location"), want)
		}
	}
	// Remove and Reset check the list first; when it can't be read, neither
	// goes ahead.
	fa = sampleAgents()
	fa.loadErr = errors.New(rawStoreClosed)
	for path, want := range map[string]string{"/agents/remove": "Couldn't remove that helper.", "/agents/reset": "Couldn't reset that helper."} {
		if _, _, e := agentsFlash(t, post(t, agentsServer(fa, cloudModels...), path, url.Values{"name": {"QA"}, "confirm": {"yes"}})); e != want || len(fa.changes()) != 0 {
			t.Errorf("%s with the list unreadable: err %q, calls %q", path, e, fa.changes())
		}
	}
	// A failed instructions read opens no editor that could save over them.
	fa = sampleAgents()
	fa.loadErr = errors.New(rawStoreClosed)
	fa.rows = sampleAgents().rows
	s := agentsServer(&soulFails{fa}, cloudModels...)
	body := get(t, s, "/agents?edit=Programmer").Body.String()
	if strings.Contains(body, `<textarea name="soul"`) || !strings.Contains(body, "Couldn&#39;t read this helper&#39;s instructions.") || strings.Contains(body, rawStoreClosed) {
		t.Errorf("a failed instructions read:\n%s", body)
	}
}

// soulFails lists fine but can't read instructions.
type soulFails struct{ *fakeAgents }

func (f *soulFails) Agents() ([]AgentProfileView, error) { return f.rows, nil }

// Every change goes through the security envelope: a browser post with the
// sign-in cookie but no Origin is refused before the control sees it, and the
// same post from the page itself goes through (so the routes are mounted).
func TestAgents_RefusesCrossSite(t *testing.T) {
	fa := sampleAgents()
	s := withSession(New(Config{Auth: "secret", Addr: "example.com"}, nil, nil, nil, nil, nil))
	s.SetAgents(fa)
	send := func(p, origin string) int {
		req := httptest.NewRequest("POST", "http://example.com"+p, strings.NewReader("name=researcher&provider=cloud&new_name=x&soul=x&confirm=yes"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: dashCookieName, Value: testSession})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		s.served().ServeHTTP(rec, req)
		return rec.Code
	}
	for _, p := range []string{"/agents/add", "/agents/model", "/agents/soul", "/agents/rename", "/agents/remove", "/agents/reset"} {
		if code := send(p, ""); code != http.StatusForbidden {
			t.Errorf("%s without an Origin = %d, want 403", p, code)
		}
	}
	if len(fa.changes()) != 0 {
		t.Errorf("the control was called: %q", fa.changes())
	}
	if code := send("/agents/remove", "http://example.com"); code != http.StatusSeeOther || !slices.Equal(fa.changes(), []string{"Remove|researcher"}) {
		t.Errorf("the page's own remove = %d, calls %q; want 303 and one Remove", code, fa.changes())
	}
}
