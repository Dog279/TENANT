package dashboard

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// liveModels, liveKeys and liveRepos change when set, as the real controls
// do. A browser test's server and the test share them.
type liveModels struct {
	mu sync.Mutex
	fakeModelCtl
}

func (f *liveModels) Models() []ModelView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.models)
}

func (f *liveModels) Use(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.models {
		f.models[i].Active = f.models[i].Name == name
	}
	return "switched to " + name, nil
}

type liveKeys struct {
	mu sync.Mutex
	fakeSecrets
}

func (f *liveKeys) List() []ServiceKeyView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.views)
}

func (f *liveKeys) SetSecret(id, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, id+"="+v)
	for i := range f.views {
		f.views[i].Set = f.views[i].Set || f.views[i].CredID == id
	}
	return nil
}

type liveRepos struct {
	mu sync.Mutex
	fakeGitHubRepos
}

func (f *liveRepos) Repos() ([]GitHubRepoRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows), nil
}

func (f *liveRepos) Remove(repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rmCalls = append(f.rmCalls, repo)
	f.rows = slices.DeleteFunc(f.rows, func(r GitHubRepoRow) bool { return r.Slug == repo })
	return nil
}

// Models, Provider keys and Integrations answer in place (DESIGN.md "Forms
// that don't reload") on a phone, signed in through the form. On each page a
// form well down it keeps the scroll and the page (a marker set before it
// survives), shows its toast, and the row shows the change; the page fits.
// A key typed into Provider keys is gone from its box once stored, and a
// double tap on Add stores it once. A double tap on Stop watching stops one
// repository, though the second tap lands on the same form holding the next.
func TestSettingsInlineFormsInChrome(t *testing.T) {
	models := &liveModels{fakeModelCtl: fakeModelCtl{ceiling: 30}}
	keys := &liveKeys{}
	repos := &liveRepos{}
	integ := &fakeIntegrationsCtl{fields: map[string][]IntegrationField{}}
	for i := range 12 {
		models.models = append(models.models, ModelView{Name: fmt.Sprintf("m%d", i), Kind: "openai", Model: "gpt", Active: i == 11})
		keys.views = append(keys.views, ServiceKeyView{CredID: fmt.Sprintf("k%d", i), Name: fmt.Sprintf("Service %d", i), Category: "Model"})
		repos.rows = append(repos.rows, GitHubRepoRow{Slug: fmt.Sprintf("o/r%d", i)})
	}
	for i := range 4 {
		id := fmt.Sprintf("i%d", i)
		integ.list = append(integ.list, IntegrationView{ID: id, Label: "Service " + id})
		integ.fields[id] = []IntegrationField{{Key: "token", Prompt: "Token", Secret: true}, {Key: "url", Prompt: "Address"}}
	}
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, nil, nil)
	s.SetModels(models)
	s.SetSecrets(keys)
	s.SetIntegrations(integ)
	s.SetGitHubRepos(repos)
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/models")

	q := strconv.Quote // a Go-quoted ASCII string is a JS string literal
	wait := chromedp.WithPollingTimeout(6 * time.Second)
	const where = `[document.querySelector('.page').scrollTop, scrollY]`
	// open visits a page and waits for Datastar: its first status poll is back.
	open := func(page string) chromedp.Action {
		return chromedp.Tasks{
			chromedp.Navigate(srv.URL + page),
			chromedp.Poll(`performance.getEntriesByType('resource').some(e => e.name.includes('/ui/status'))`, nil, wait),
		}
	}
	// center scrolls sel to the middle of the screen and marks the window.
	center := func(sel string, at *[]float64) chromedp.Action {
		return chromedp.Evaluate(`document.querySelector(`+q(sel)+`).scrollIntoView({block: 'center'}); window.inlineMark = 'kept'; `+where, at)
	}
	// doubleTap clicks the middle of sel twice at one spot, 120ms apart.
	doubleTap := func(sel string) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			var xy []float64
			if err := chromedp.Evaluate(`(r => [r.left + r.width / 2, r.top + r.height / 2])(document.querySelector(`+q(sel)+`).getBoundingClientRect())`, &xy).Do(ctx); err != nil {
				return err
			}
			if err := chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx); err != nil {
				return err
			}
			time.Sleep(120 * time.Millisecond)
			return chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx)
		})
	}
	toastSays := func(text string) chromedp.Action {
		return chromedp.Poll(`(t => t.classList.contains('ok') && t.textContent.includes(`+q(text)+`))(document.getElementById('toast'))`, nil, wait)
	}
	type view struct {
		before, after []float64
		mark, row     string
		bad           []string
	}
	// answered waits for the toast, then reads the scroll, the marker, row's
	// text and what doesn't fit.
	answered := func(v *view, toast, row string) chromedp.Action {
		return chromedp.Tasks{
			toastSays(toast),
			chromedp.Sleep(300 * time.Millisecond), // room for a second post to land
			toastSays(toast),                       // and no other answer replaced it
			chromedp.Evaluate(where, &v.after),
			chromedp.Evaluate(`String(window.inlineMark)`, &v.mark),
			chromedp.Text(row, &v.row, chromedp.ByQuery),
			chromedp.Evaluate(fitsJS, &v.bad),
		}
	}
	kept := func(step string, v view) {
		t.Helper()
		if len(v.before) != 2 || v.before[0] < 100 {
			t.Errorf("%s: the page scrolled to %v before, want well down it", step, v.before)
		}
		if fmt.Sprint(v.after) != fmt.Sprint(v.before) {
			t.Errorf("%s: the scroll moved from %v to %v", step, v.before, v.after)
		}
		if v.mark != "kept" {
			t.Errorf("%s: the page reloaded: the marker is %q", step, v.mark)
		}
		for _, b := range v.bad {
			t.Errorf("%s at 390px: %s", step, b)
		}
	}

	// Models: Use this model on a row far down, through its confirm step.
	var use view
	var was string
	err := chromedp.Run(ctx,
		open("/models"),
		center(`#model-m10`, &use.before),
		chromedp.Click(`#model-m10 details.go > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(`#model-m10 details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.Click(`#model-m10 details[open] .confirm-box button`, chromedp.ByQuery),
		answered(&use, "Every answer now comes from m10.", `#model-m10`),
		chromedp.Text(`#model-m11`, &was, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	kept("Models", use)
	if !strings.Contains(use.row, "in use") || strings.Contains(was, "in use") {
		t.Errorf("Models: the rows don't show the switch: m10 %q, m11 %q", use.row, was)
	}

	// Provider keys: a key typed far down, and a double tap on Add.
	const secret = "sk-typed-SENTINEL"
	var set view
	var box string
	err = chromedp.Run(ctx,
		open("/settings/keys"),
		chromedp.SendKeys(`#key-k6 input[name=value]`, secret, chromedp.ByQuery),
		center(`#key-k6`, &set.before),
		doubleTap(`#key-k6 button[type=submit]`),
		answered(&set, "Key stored.", `#key-k6`),
		chromedp.Evaluate(`document.querySelector('#key-k6 input[name=value]').value`, &box),
	)
	if err != nil {
		t.Fatalf("Provider keys: %v", err)
	}
	kept("Provider keys", set)
	keys.mu.Lock()
	stored := slices.Clone(keys.setCalls)
	keys.mu.Unlock()
	if fmt.Sprint(stored) != "[k6="+secret+"]" {
		t.Errorf("a double tap on Add stored %q, want it stored once", stored)
	}
	if !strings.Contains(set.row, "stored") || !strings.Contains(set.row, "Replace") {
		t.Errorf("Provider keys: the row doesn't show the key stored: %q", set.row)
	}
	if box != "" {
		t.Errorf("the key stayed in its box: %q", box)
	}

	// Integrations: a double tap on the first repository's Stop watching.
	var stop view
	var slugs []string
	err = chromedp.Run(ctx,
		open("/integrations"),
		center(`#github-repos .row`, &stop.before),
		doubleTap(`#github-repos .row form[action="/integrations/github/repos/remove"] button`),
		answered(&stop, "Stopped watching o/r0.", `#github-repos .row`),
		chromedp.Evaluate(`[...document.querySelectorAll('#github-repos .row .nm')].map(e => e.textContent)`, &slugs),
	)
	if err != nil {
		t.Fatalf("Integrations: %v", err)
	}
	kept("Integrations", stop)
	repos.mu.Lock()
	removed := slices.Clone(repos.rmCalls)
	repos.mu.Unlock()
	if fmt.Sprint(removed) != "[o/r0]" || len(slugs) != 11 || slugs[0] != "o/r1" {
		t.Errorf("a double tap on Stop watching removed %v and left %v, want o/r0 alone gone", removed, slugs)
	}
}
