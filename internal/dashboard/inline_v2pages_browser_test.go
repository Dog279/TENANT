package dashboard

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// shrinkingAgents is fakeAgents whose Remove takes the helper off the list,
// as the real control does. The browser's server and the test share it.
type shrinkingAgents struct {
	mu sync.Mutex
	*fakeAgents
}

func (f *shrinkingAgents) Agents() ([]AgentProfileView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows), nil
}

func (f *shrinkingAgents) Add(n, p, m, d string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fakeAgents.Add(n, p, m, d)
}

func (f *shrinkingAgents) Remove(n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = slices.DeleteFunc(f.rows, func(a AgentProfileView) bool { return a.Name == n })
	return f.fakeAgents.Remove(n)
}

func (f *shrinkingAgents) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fakeAgents.changes()
}

// Approvals & safety, Helpers, Research and Autopilot answer their forms in
// place on a phone (390x844), signed in through the form, with the harness's
// long pages. Far down each page a change keeps the scroll and the page (a
// marker set before it survives), its toast says what happened, and the row
// shows it. A double tap at one spot posts once where a second post would
// do something: Deny, Add helper, Run again and Stop. On Research the live
// panel still streams afterward, and a started question leaves the box
// empty. TENANT_UI_SCREENSHOT=<dir> keeps screenshots.
func TestV2PagesInlineInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	fs, fr, fg := s.safety.(*fakeSafety), s.research.(*fakeResearch), s.goal.(*fakeGoal)
	fa := &shrinkingAgents{fakeAgents: s.agents.(*fakeAgents)}
	s.SetAgents(fa)
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/safety")

	q := strconv.Quote // a Go-quoted ASCII string is a JS string literal
	wait := chromedp.WithPollingTimeout(6 * time.Second)
	shot := func(name string) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			dir := os.Getenv("TENANT_UI_SCREENSHOT")
			if dir == "" {
				return nil
			}
			var png []byte
			if err := chromedp.CaptureScreenshot(&png).Do(ctx); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "v2pages-390-"+name+".png"), png, 0o644)
		})
	}
	// open goes to page and waits for Datastar: the status strip fills.
	open := func(page string) chromedp.Action {
		return chromedp.Tasks{
			chromedp.Navigate(srv.URL + page),
			chromedp.Poll(`document.getElementById('ui-status').textContent.includes('waiting for your OK')`, nil, wait),
		}
	}
	toastSays := func(text string) chromedp.Action {
		return chromedp.Poll(`(t => t.classList.contains('ok') && t.textContent.includes(`+q(text)+`))(document.getElementById('toast'))`, nil, wait)
	}
	// center puts sel's element in the middle of the screen, sets the marker
	// a reload would lose, and notes where keep's element is on the screen
	// and how far down the page is.
	center := func(sel, keep string, at *[]float64) chromedp.Action {
		return chromedp.Evaluate(`document.querySelector(`+q(sel)+`).scrollIntoView({block: 'center'}); window.inlineMark = 'kept'; `+
			`[document.querySelector(`+q(keep)+`).getBoundingClientRect().top, document.querySelector('.page').scrollTop]`, at)
	}
	// doubleTap taps the middle of sel's element twice, 120ms apart, at one
	// spot.
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
	settle := chromedp.Sleep(300 * time.Millisecond) // room for a second post to land
	// stayed checks that the page wasn't reloaded and that keep's element is
	// where it was on the screen: the scroll was kept. (scrollTop itself may
	// move by what changed above it: Chrome's scroll anchoring holds what is
	// on the screen still.) The page fits with the toast.
	stayed := func(page, keep string, at *[]float64) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			var top float64
			var mark string
			var bad []string
			if err := chromedp.Run(ctx,
				chromedp.Evaluate(`document.querySelector(`+q(keep)+`).getBoundingClientRect().top`, &top),
				chromedp.Evaluate(`String(window.inlineMark)`, &mark),
				chromedp.Evaluate(fitsJS, &bad)); err != nil {
				return err
			}
			if mark != "kept" {
				t.Errorf("%s reloaded: the marker is %q", page, mark)
			}
			if before := *at; len(before) != 2 || before[1] < 100 {
				t.Errorf("%s was at %v before the tap, want well down it", page, before)
			} else if math.Abs(top-before[0]) > 1 {
				t.Errorf("%s moved: %s went from %vpx to %vpx on the screen", page, keep, before[0], top)
			}
			for _, b := range bad {
				t.Errorf("%s with the toast: %s", page, b)
			}
			return nil
		})
	}

	// Approvals & safety: Deny on the last mode, double-tapped.
	var before []float64
	var row string
	err := chromedp.Run(ctx,
		open("/safety"),
		center(`#perm-sandbox`, `#perm-sandbox`, &before),
		doubleTap(`#perm-sandbox form:has(button[value="deny"]) button`),
		toastSays("Leave the sandbox: your agent can't do this now."),
		settle,
		shot("safety"),
		stayed("/safety", `#perm-sandbox`, &before),
		chromedp.Text(`#perm-sandbox`, &row, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if !strings.Contains(row, "Can't run a command outside the safety box after the box stopped it. It won't ask.") || strings.Contains(row, "New") {
		t.Errorf("the row doesn't show its new mode: %q", row)
	}
	if c := fs.called(); !slices.Equal(c, []string{"dashboard|sandbox|deny"}) {
		t.Errorf("a double tap on Deny sent %q, want one deny", c)
	}

	// Helpers: Remove, far down the list, through its confirm step; then
	// Add helper, double-tapped, adds once and leaves the form empty.
	name, above := longName(11), `#agent-`+longName(10) // the operator's own, and the row above it
	var typed string
	err = chromedp.Run(ctx,
		open("/agents"),
		center(`#agent-`+name, above, &before),
		chromedp.Click(`#agent-`+name+` details.confirm-step > summary`, chromedp.ByQuery),
		chromedp.Click(`#agent-`+name+` details[open] .confirm-box button`, chromedp.ByQuery),
		toastSays("Removed "+name+"."),
		chromedp.WaitNotPresent(`#agent-`+name, chromedp.ByQuery),
		shot("helpers"),
		stayed("/agents", above, &before),
		chromedp.SendKeys(`form[action="/agents/add"] input[name=name]`, "newhelper", chromedp.ByQuery),
		center(`form[action="/agents/add"] button`, `form[action="/agents/add"] button`, &before),
		doubleTap(`form[action="/agents/add"] button`),
		toastSays("Added newhelper."),
		settle,
		stayed("/agents", `form[action="/agents/add"] button`, &before),
		chromedp.Value(`form[action="/agents/add"] input[name=name]`, &typed, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Helpers: %v", err)
	}
	if c := fa.changes(); len(c) != 2 || c[0] != "Remove|"+name || !strings.HasPrefix(c[1], "Add|newhelper|") || typed != "" {
		t.Errorf("calls %q and %q left in the name box; want one Remove, one Add and an empty box", c, typed)
	}

	// Research: Delete in the middle of the list, through its confirm step;
	// Run again, double-tapped, runs once; the live panel still streams; a
	// started question leaves the box empty.
	var question string
	err = chromedp.Run(ctx,
		open("/research"),
		chromedp.WaitVisible(`#research-live .research-run`, chromedp.ByQuery),
		center(`.research-row:has(input[value="r-6"])`, `.research-row:has(input[value="r-5"])`, &before),
		chromedp.Click(`.research-row:has(input[value="r-6"]) details.confirm-step > summary`, chromedp.ByQuery),
		chromedp.Click(`.research-row:has(input[value="r-6"]) details[open] .confirm-box button`, chromedp.ByQuery),
		toastSays("Deleted from the list."),
		chromedp.WaitNotPresent(`input[value="r-6"]`, chromedp.ByQuery),
		shot("research"),
		stayed("/research", `.research-row:has(input[value="r-5"])`, &before),
		doubleTap(`.research-row:has(input[value="r-8"]) form[action="/research/replay"] button`),
		toastSays("Running it again with the current model."),
		settle,
		chromedp.ActionFunc(func(context.Context) error {
			fr.set(ResearchLiveView{Phase: "writing", Running: true, Question: longName(0), Elapsed: "5 min"})
			return nil
		}),
		chromedp.Poll(`document.getElementById('research-live').textContent.includes('Writing the report')`, nil, wait),
		chromedp.SendKeys(`textarea[name=question]`, "What is new?", chromedp.ByQuery),
		chromedp.Click(`form[action="/research/start"] button`, chromedp.ByQuery),
		toastSays("Research started."),
		chromedp.Value(`textarea[name=question]`, &question, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if c := fr.called(); !slices.Equal(c, []string{"delete r-6", "replay r-8", `start "What is new?" false`}) || question != "" {
		t.Errorf("calls %q and %q left in the question box; want one each and an empty box", c, question)
	}

	// Autopilot: Stop, double-tapped, stops once.
	var kept string
	err = chromedp.Run(ctx,
		open("/goal"),
		chromedp.Evaluate(`window.inlineMark = 'kept'`, nil),
		doubleTap(`form[action="/goal/stop"] button`),
		toastSays("Stopped. The step in progress finishes, then no new step starts."),
		chromedp.WaitVisible(`#goal-body .empty`, chromedp.ByQuery),
		shot("goal"),
		settle,
		chromedp.Evaluate(`String(window.inlineMark)`, &kept),
	)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := fg.clearCalls(); n != 1 || kept != "kept" {
		t.Errorf("a double tap on Stop: %d Clear calls, marker %q; want one, and no reload", n, kept)
	}
}
