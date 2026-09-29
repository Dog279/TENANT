package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

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

func (f *liveRepos) SetWrite(repo string, write bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, fmt.Sprintf("%s:%v", repo, write))
	for i := range f.rows {
		f.rows[i].Write = f.rows[i].Write || f.rows[i].Slug == repo && write
	}
	return nil
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
// A double tap on Allow changes' Yes, with another repository's control under
// it once the confirm box closes, allows the one repository and nothing else.
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

	type view struct {
		at  []float64 // markPlace's
		row string
	}
	// answered waits for the toast and for any second post, checks the page
	// stayed with row where it was, and reads row's text.
	answered := func(step string, v *view, toast, row string) chromedp.Action {
		return chromedp.Tasks{
			toastSays("ok", toast),
			tapsOpen,
			toastSays("ok", toast), // and no other answer replaced it
			stayed(t, step, row, &v.at),
			chromedp.Text(row, &v.row, chromedp.ByQuery),
		}
	}

	// Models: Use this model on a row far down, through its confirm step.
	var use view
	var was string
	err := run(ctx,
		visit(srv.URL+"/models"),
		markPlace(`#model-m10`, `#model-m10`, &use.at),
		chromedp.Click(`#model-m10 details.go > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(`#model-m10 details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.Click(`#model-m10 details[open] .confirm-box button`, chromedp.ByQuery),
		answered("Models", &use, "Every answer now comes from m10.", `#model-m10`),
		chromedp.Text(`#model-m11`, &was, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !strings.Contains(use.row, "in use") || strings.Contains(was, "in use") {
		t.Errorf("Models: the rows don't show the switch: m10 %q, m11 %q", use.row, was)
	}

	// Provider keys: a key typed far down, and a double tap on Add.
	const secret = "sk-typed-SENTINEL"
	var set view
	var box string
	err = run(ctx,
		visit(srv.URL+"/settings/keys"),
		chromedp.SendKeys(`#key-k6 input[name=value]`, secret, chromedp.ByQuery),
		markPlace(`#key-k6`, `#key-k6`, &set.at),
		doubleTap(`#key-k6 button[type=submit]`),
		answered("Provider keys", &set, "Key stored.", `#key-k6`),
		chromedp.Evaluate(`document.querySelector('#key-k6 input[name=value]').value`, &box),
	)
	if err != nil {
		t.Fatalf("Provider keys: %v", err)
	}
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
	// The first row is the one tapped, then the one after it in its place.
	err = run(ctx,
		visit(srv.URL+"/integrations"),
		markPlace(`#github-repos .row`, `#github-repos .row`, &stop.at),
		doubleTap(`#github-repos .row form[action="/integrations/github/repos/remove"] button`),
		answered("Integrations", &stop, "Stopped watching o/r0.", `#github-repos .row`),
		chromedp.Evaluate(`[...document.querySelectorAll('#github-repos .row .nm')].map(e => e.textContent)`, &slugs),
	)
	if err != nil {
		t.Fatalf("Integrations: %v", err)
	}
	repos.mu.Lock()
	removed := slices.Clone(repos.rmCalls)
	repos.mu.Unlock()
	if fmt.Sprint(removed) != "[o/r0]" || len(slugs) != 11 || slugs[0] != "o/r1" {
		t.Errorf("a double tap on Stop watching removed %v and left %v, want o/r0 alone gone", removed, slugs)
	}

	// Integrations: a double tap on Allow changes' Yes at a spot where, once
	// the answer closes the phone's confirm box, another repository's control
	// lies under the finger.
	const r4 = `#github-repos .row:has(input[name=repo][value="o/r4"])`
	// under finds that spot on Yes: one with another row's button beneath
	// the confirm box, or failing that its Allow changes.
	const under = `(() => {
		const yes = document.querySelector('#github-repos details[open] .confirm-box button'), box = yes.closest('.confirm-box'), r = yes.getBoundingClientRect(), y = r.top + r.height / 2;
		box.style.visibility = 'hidden';
		let at = null;
		for (const sel of ['#github-repos .row form button', '#github-repos .row summary']) {
			for (let x = r.left + 2; x < r.right - 2 && !at; x += 4) {
				if (document.elementFromPoint(x, y)?.closest(sel)) at = [x, y];
			}
		}
		box.style.visibility = '';
		return at;
	})()`
	var allow view
	var opened int
	err = run(ctx,
		visit(srv.URL+"/integrations"),
		markPlace(r4, r4, &allow.at),
		chromedp.Click(r4+` details.go > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(r4+` details[open] .confirm-box button`, chromedp.ByQuery),
		tapTwice(under),
		answered("Integrations, Allow changes", &allow, "Your agent may now change o/r4.", r4),
		chromedp.Evaluate(`document.querySelectorAll('#github-repos details[open]').length`, &opened),
	)
	repos.mu.Lock()
	modes := slices.Clone(repos.setCalls)
	removed = slices.Clone(repos.rmCalls)
	repos.mu.Unlock()
	if err != nil {
		t.Fatalf("Integrations, Allow changes: %v (set %v, removed %v)", err, modes, removed)
	}
	if fmt.Sprint(modes) != "[o/r4:true]" || fmt.Sprint(removed) != "[o/r0]" || opened != 0 {
		t.Errorf("a double tap on Yes, allow changes set %v, removed %v and opened %d confirm steps, want o/r4 allowed and nothing else", modes, removed, opened)
	}
	if !strings.Contains(allow.row, "may change it") {
		t.Errorf("the row doesn't show the change allowed: %q", allow.row)
	}
}
