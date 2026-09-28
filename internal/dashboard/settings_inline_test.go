package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// settingsPages is where the Models, Provider keys and Integrations forms
// post: every one a literal URL (F1), with the row's values in fields.
var settingsPages = map[string][]string{
	"/models":        {"/models/use", "/models/remove", "/models/add", "/models/ceiling", "/models/reload"},
	"/settings/keys": {"/settings/keys/set", "/settings/keys/remove"},
	"/integrations": {"/integrations/save", "/integrations/test", "/integrations/disconnect",
		"/integrations/github/repos/add", "/integrations/github/repos/mode", "/integrations/github/repos/remove"},
}

// Every form on Models, Provider keys and Integrations answers a Datastar
// post in place: the toast, the status strip and the page's region, never a
// 303 the fetch would follow, and no key in the answer. With no control the
// region says so. A plain post still gets the 303 and its flash.
func TestSettingsGroup_InlineAnswersEveryForm(t *testing.T) {
	const secret = "sk-SENTINEL-inline"
	brave := map[string][]IntegrationField{"brave": {{Key: "api_key", Secret: true}}}
	for _, c := range []struct {
		route, page, region string
		form                url.Values
		wire                func(*Server)
	}{
		{"/models/use", "/models", "models-body", url.Values{"name": {"m"}}, nil},
		{"/models/add", "/models", "models-body", url.Values{"kind": {"anthropic"}, "key": {secret}}, nil},
		{"/models/remove", "/models", "models-body", url.Values{"name": {"m"}}, nil},
		{"/models/ceiling", "/models", "models-body", url.Values{"ceiling": {"42"}}, nil},
		{"/models/reload", "/models", "models-body", url.Values{}, nil},
		{"/settings/keys/set", "/settings/keys", "keys-body", url.Values{"service": {"openai"}, "value": {secret}}, nil},
		{"/settings/keys/remove", "/settings/keys", "keys-body", url.Values{"service": {"openai"}}, nil},
		{"/integrations/save", "/integrations", "integrations-body", url.Values{"integration": {"brave"}, "api_key": {secret}}, nil},
		{"/integrations/test", "/integrations", "integrations-body", url.Values{"integration": {"brave"}}, nil},
		{"/integrations/disconnect", "/integrations", "integrations-body", url.Values{"integration": {"brave"}}, nil},
		{"/integrations/github/repos/add", "/integrations", "integrations-body", url.Values{"repo": {"a/b"}}, nil},
		{"/integrations/github/repos/mode", "/integrations", "integrations-body", url.Values{"repo": {"a/b"}, "mode": {"write"}}, nil},
		{"/integrations/github/repos/remove", "/integrations", "integrations-body", url.Values{"repo": {"a/b"}}, nil},
	} {
		for _, wired := range []bool{true, false} {
			s := New(Config{}, nil, nil, nil, nil, nil)
			if wired {
				s.SetModels(&fakeModelCtl{})
				s.SetSecrets(&fakeSecrets{})
				s.SetIntegrations(&fakeIntegrationsCtl{fields: brave})
				s.SetGitHubRepos(&fakeGitHubRepos{})
			}
			form := withField(c.form, "confirm", "yes")
			rec := sendForm(s.Handler(), http.MethodPost, c.route, form, datastar)
			body := rec.Body.String()
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Location") != "" {
				t.Errorf("%s (wired %v): %d %q to %q, want 200 text/event-stream", c.route, wired, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Location"))
			}
			for _, want := range []string{"data: mode replace\ndata: elements <div id=\"toast\"", `data: elements <div id="ui-status"`, `data: elements <div id="` + c.region + `">`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s (wired %v) is missing %q:\n%s", c.route, wired, want, body)
				}
			}
			if wired && !strings.Contains(body, `<div id="toast" class="toast ok">`) {
				t.Errorf("%s: want a success toast:\n%s", c.route, body)
			}
			if !wired && !strings.Contains(body, `class="card state-off"`) {
				t.Errorf("%s with no control: the region doesn't say so:\n%s", c.route, body)
			}
			if strings.Contains(body, secret) {
				t.Errorf("%s (wired %v) sent the key back:\n%s", c.route, wired, body)
			}
			rec = sendForm(s.Handler(), http.MethodPost, c.route, form, nil)
			if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, c.page) || (wired && !strings.Contains(loc, "?msg=")) {
				t.Errorf("%s (wired %v), a plain post: %d to %q, want 303 to %s with its flash", c.route, wired, rec.Code, loc, c.page)
			}
		}
	}
}

// A failure answers in place with an alert and leaves the region alone, so
// what was typed stays; a key never comes back, in the toast or anywhere.
func TestSettingsGroup_InlineErrorsLeaveThePage(t *testing.T) {
	const secret = "sk-SENTINEL-err"
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetModels(&failingModelCtl{})
	s.SetSecrets(&fakeSecrets{setErr: errors.New("bad key " + secret)})
	s.SetIntegrations(&failingIntegrations{fakeIntegrationsCtl{fields: map[string][]IntegrationField{"brave": {{Key: "api_key", Secret: true}}}}})
	s.SetGitHubRepos(&fakeGitHubRepos{mutateErr: errors.New("config is locked")})
	for _, c := range []struct{ route, region, want string }{
		{"/models/add?kind=anthropic&key=" + secret, "models-body", "Nothing was saved: submit it in the form, not the URL."},
		{"/models/add?kind=anthropic", "models-body", "Couldn&#39;t add that model. The Logs page has the reason."},
		{"/models/ceiling", "models-body", "Enter a whole number greater than zero."},
		{"/settings/keys/set?service=openai", "keys-body", "Couldn&#39;t store that key."},
		{"/settings/keys/set?service=openai&value=" + secret, "keys-body", "Nothing was saved: submit it in the form, not the URL."},
		{"/integrations/save?integration=brave", "integrations-body", "Couldn&#39;t save that. The Logs page has the reason."},
		{"/integrations/github/repos/remove?repo=a/b", "integrations-body", "Couldn&#39;t stop watching a/b. Is it on the list?"},
	} {
		form := url.Values{"key": {secret}, "value": {secret}, "api_key": {secret}, "ceiling": {"0"}}
		body := sendForm(s.Handler(), http.MethodPost, c.route, form, datastar).Body.String()
		if !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">`+c.want+`</span>`) {
			t.Errorf("%s: want the alert %q:\n%s", c.route, c.want, body)
		}
		if strings.Contains(body, `id="`+c.region+`"`) || strings.Contains(body, secret) || strings.Contains(body, "locked") {
			t.Errorf("%s: a failure re-rendered the page or said too much:\n%s", c.route, body)
		}
	}
}

// A switch to a model that can't answer yet is an alert, and the rows show
// the switch, which did happen.
func TestSettingsGroup_ModelSwitchWarnsInPlace(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetModels(&warnModelCtl{fakeModelCtl{models: []ModelView{{Name: "m", Active: true}}}})
	body := sendForm(s.Handler(), http.MethodPost, "/models/use", url.Values{"name": {"m"}, "confirm": {"yes"}}, datastar).Body.String()
	for _, want := range []string{`class="toast err" role="alert"><span class="toast-text">Switched to m, but it isn&#39;t ready to answer.`, `data: elements <div id="models-body">`, "in use"} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "UNREACHABLE") {
		t.Errorf("the control's own words reached the page:\n%s", body)
	}
}

// Every form on these pages posts in place to its own literal URL,
// throttled and never sent twice, and a form with a key in it empties
// itself when its answer arrives. The populated pages carry hostile names,
// so a row's value in a URL would show here.
func TestSettingsGroup_EveryFormPostsInPlace(t *testing.T) {
	s := harnessServer(t, Config{}, populated, nil)
	form := regexp.MustCompile(`<form method="POST" action="([^"]*)"[^>]*>`)
	const empties = `data-on:datastar-fetch="evt.detail.el === el && evt.detail.type === 'finished' && el.reset()"`
	for page, routes := range settingsPages {
		seen := map[string]bool{}
		for _, m := range form.FindAllStringSubmatch(get(t, s, page).Body.String(), -1) {
			action, tag := m[1], strings.ReplaceAll(m[0], `\/`, "/")
			if action == "/auth/logout" {
				continue
			}
			seen[action] = true
			if !slices.Contains(routes, action) {
				t.Errorf("%s: a form posts to %q, not one of the page's literal URLs", page, action)
			}
			if !strings.Contains(tag, `data-on:submit__prevent__throttle.1s="@post('`+action+`', {contentType:'form', retryMaxCount:0})"`) {
				t.Errorf("%s: %s doesn't post in place, throttled and once", page, tag)
			}
			if secretForm := action == "/models/add" || action == "/settings/keys/set" || action == "/integrations/save"; secretForm != strings.Contains(tag, empties) {
				t.Errorf("%s: %s: empties itself %v, want %v", page, tag, !secretForm, secretForm)
			}
		}
		for _, r := range routes {
			if !seen[r] {
				t.Errorf("%s has no form for %s", page, r)
			}
		}
	}
}
