package dashboard

import (
	"context"
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
// do something: Deny, Add helper, Run again, Stop, and a Delete step's Yes,
// whose sheet closes under the finger (layout.html's tap guard keeps the
// second tap off the Run again below it). On Research the live
// panel still streams afterward, and a started question leaves the box
// empty. TENANT_UI_SCREENSHOT=<dir> keeps screenshots.
func TestV2PagesInlineInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	fs, fr, fg := s.safety.(*fakeSafety), s.research.(*fakeResearch), s.goal.(*fakeGoal)
	fa := &shrinkingAgents{fakeAgents: s.agents.(*fakeAgents)}
	s.SetAgents(fa)
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/safety")

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
	// doubleTap taps sel's element twice at one spot 10px in from its right
	// end, once the tap guard is down.
	doubleTap := func(sel string) chromedp.Action {
		return chromedp.Tasks{tapsOpen, tapTwice(`(r => [r.right - 10, r.top + r.height / 2])(document.querySelector(` + strconv.Quote(sel) + `).getBoundingClientRect())`)}
	}

	// Approvals & safety: Deny on the last mode, double-tapped.
	var before []float64
	var row string
	err := run(ctx,
		visit(srv.URL+"/safety"),
		markPlace(`#perm-sandbox`, `#perm-sandbox`, &before),
		doubleTap(`#perm-sandbox form:has(button[value="deny"]) button`),
		toastSays("ok", "Leave the sandbox: your agent can't do this now."),
		tapsOpen,
		shot("safety"),
		stayed(t, "Deny", `#perm-sandbox`, &before),
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
	err = run(ctx,
		visit(srv.URL+"/agents"),
		markPlace(`#agent-`+name, above, &before),
		chromedp.Click(`#agent-`+name+` details.confirm-step > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.Click(`#agent-`+name+` details[open] .confirm-box button`, chromedp.ByQuery),
		toastSays("ok", "Removed "+name+"."),
		chromedp.WaitNotPresent(`#agent-`+name, chromedp.ByQuery),
		shot("helpers"),
		stayed(t, "Remove", above, &before),
		chromedp.SendKeys(`form[action="/agents/add"] input[name=name]`, "newhelper", chromedp.ByQuery),
		markPlace(`form[action="/agents/add"] button`, `form[action="/agents/add"] button`, &before),
		doubleTap(`form[action="/agents/add"] button`),
		toastSays("ok", "Added newhelper."),
		tapsOpen,
		stayed(t, "Add helper", `form[action="/agents/add"] button`, &before),
		chromedp.Value(`form[action="/agents/add"] input[name=name]`, &typed, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Helpers: %v", err)
	}
	if c := fa.changes(); len(c) != 2 || c[0] != "Remove|"+name || !strings.HasPrefix(c[1], "Add|newhelper|") || typed != "" {
		t.Errorf("calls %q and %q left in the name box; want one Remove, one Add and an empty box", c, typed)
	}

	// Research: Delete in the middle of the list, its confirm step's Yes
	// double-tapped, deletes once and runs nothing; Run again, double-tapped,
	// runs once; the live panel still streams; a started question leaves the
	// box empty.
	var question string
	var afterDelete []string
	err = run(ctx,
		visit(srv.URL+"/research"),
		chromedp.WaitVisible(`#research-live .research-run`, chromedp.ByQuery),
		markPlace(`.research-row:has(input[value="r-6"])`, `.research-row:has(input[value="r-5"])`, &before),
		chromedp.Click(`.research-row:has(input[value="r-6"]) details.confirm-step > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		doubleTap(`.research-row:has(input[value="r-6"]) details[open] .confirm-box button`),
		toastSays("ok", "Deleted from the list."),
		tapsOpen,
		chromedp.ActionFunc(func(context.Context) error { afterDelete = fr.called(); return nil }),
		chromedp.WaitNotPresent(`input[value="r-6"]`, chromedp.ByQuery),
		shot("research"),
		stayed(t, "Delete", `.research-row:has(input[value="r-5"])`, &before),
		doubleTap(`.research-row:has(input[value="r-8"]) form[action="/research/replay"] button`),
		toastSays("ok", "Running it again with the current model."),
		tapsOpen,
		chromedp.ActionFunc(func(context.Context) error {
			fr.set(ResearchLiveView{Phase: "writing", Running: true, Question: longName(0), Elapsed: "5 min"})
			return nil
		}),
		chromedp.Poll(`document.getElementById('research-live').textContent.includes('Writing the report')`, nil, wait),
		chromedp.SendKeys(`textarea[name=question]`, "What is new?", chromedp.ByQuery),
		tapsOpen,
		chromedp.Click(`form[action="/research/start"] button`, chromedp.ByQuery),
		toastSays("ok", "Research started."),
		chromedp.Value(`textarea[name=question]`, &question, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if !slices.Equal(afterDelete, []string{"delete r-6"}) {
		t.Errorf("a double tap on Yes, delete sent %q, want the delete alone", afterDelete)
	}
	if c := fr.called(); !slices.Equal(c, []string{"delete r-6", "replay r-8", `start "What is new?" false`}) || question != "" {
		t.Errorf("calls %q and %q left in the question box; want one each and an empty box", c, question)
	}

	// Autopilot: Stop, double-tapped, stops once.
	err = run(ctx,
		visit(srv.URL+"/goal"),
		markPlace(`form[action="/goal/stop"] button`, `#goal-body`, &before),
		doubleTap(`form[action="/goal/stop"] button`),
		toastSays("ok", "Stopped. The step in progress finishes, then no new step starts."),
		chromedp.WaitVisible(`#goal-body .empty`, chromedp.ByQuery),
		shot("goal"),
		tapsOpen,
		stayed(t, "Stop", `#goal-body`, &before),
	)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := fg.clearCalls(); n != 1 {
		t.Errorf("a double tap on Stop: %d Clear calls, want one", n)
	}
}
