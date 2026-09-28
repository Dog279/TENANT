package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Research in a real browser, on a phone and on a desktop: the phases and
// then the report stream in with no reload, the report's links are only the
// web ones, nothing scrolls sideways, a re-render leaves an open Stop step
// or Details open, and Delete goes through its confirm step. TENANT_UI_SCREENSHOT=<dir> keeps the screenshots.
func TestResearchPageInChrome(t *testing.T) {
	for _, w := range []int{390, 1440} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			f := newFakeResearch()
			f.rows = []ResearchRunRow{{ID: "r-1", Question: longName(1), StateText: "Finished", When: "Sep 27, 14:05", Took: "4 min", Sources: 3}}
			s := researchServer(f)
			s.cfg.Addr, s.cfg.Auth = "127.0.0.1:0", GenerateToken()
			ctx, srv := chromeSignedIn(t, s, w, 844, "/research")
			shoot := func(name string) chromedp.Action {
				return chromedp.ActionFunc(func(ctx context.Context) error {
					dir := os.Getenv("TENANT_UI_SCREENSHOT")
					if dir == "" {
						return nil
					}
					var png []byte
					if err := chromedp.FullScreenshot(&png, 80).Do(ctx); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, fmt.Sprintf("research-%s-%d.jpg", name, w)), png, 0o644)
				})
			}
			set := func(v ResearchLiveView) chromedp.Action {
				return chromedp.ActionFunc(func(context.Context) error { f.set(v); return nil })
			}
			has := func(sel, text string) chromedp.Action {
				return chromedp.Poll(fmt.Sprintf(`(document.querySelector(%q) || {textContent: ''}).textContent.includes(%q)`, sel, text), nil, chromedp.WithPollingTimeout(5*time.Second))
			}
			var mark, open int
			var jsLinks int
			var boldTag bool
			var bad []string
			lines := []string{"A helper finished: " + longURL(4)}
			err := chromedp.Run(ctx,
				chromedp.Evaluate(`window.__mark = 1`, nil),
				set(ResearchLiveView{Phase: "researching", Running: true, Question: longName(2), Round: 1, Rounds: 2,
					Questions: []string{"What is A?", longURL(3)}, Progress: "Questions 1-2 are in progress.", Elapsed: "less than a minute", Lines: lines}),
				has("#research-live", "Round 1 of 2"),
				chromedp.Evaluate(fitsJS, &bad),
				shoot("running"),
				chromedp.Click(`#research-live details.confirm-step > summary`, chromedp.ByQuery),
				chromedp.Click(`#research-live details.research-lines > summary`, chromedp.ByQuery),
				set(ResearchLiveView{Phase: "writing", Running: true, Question: longName(2), Elapsed: "4 min", Lines: lines}),
				has("#research-live", "Writing the report"),
				chromedp.Evaluate(`document.querySelectorAll('#research-live details[open]').length`, &open),
				set(ResearchLiveView{Phase: "done", RunID: "r-2", Report: &ResearchReportView{ID: "r-2", Question: longName(2), StateText: "Finished",
					Body: "# Fake report\nAnswer [1]. <b>not bold</b> [x](javascript:alert(1)) " + strings.Repeat("z", 200) + " [2] [3]",
					Refs: []ResearchRef{{N: 1, URL: longURL(1)}, {N: 2, URL: "JaVaScRiPt:alert(1)"}, {N: 3, URL: "data:text/html,<script>alert(1)</script>"}}}}),
				chromedp.WaitVisible(`#research-live a.cite[href="#ref-1"]`, chromedp.ByQuery),
				chromedp.WaitVisible(`#ref-1 a[rel="noopener noreferrer nofollow"]`, chromedp.ByQuery),
				chromedp.Evaluate(`window.__mark`, &mark),
				chromedp.Evaluate(`document.querySelectorAll('a[href^="javascript" i], a[href^="data" i]').length`, &jsLinks),
				chromedp.Evaluate(`document.querySelector('.report b') === null && document.querySelector('.report').textContent.includes('<b>not bold</b>')`, &boldTag),
				chromedp.Evaluate(fitsJS, &bad),
				shoot("report"),
			)
			if err != nil {
				t.Fatalf("the live panel: %v", err)
			}
			if mark != 1 {
				t.Error("the page reloaded: the report must stream in")
			}
			if open != 2 {
				t.Errorf("%d of Stop's step and Details open after a re-render, want 2", open)
			}
			if jsLinks != 0 || !boldTag {
				t.Errorf("%d javascript: or data: links; <b> shown as text: %v", jsLinks, boldTag)
			}
			for _, b := range bad {
				t.Errorf("at %dpx: %s", w, b)
			}
			// A dropped stream shows the paused banner; the reconnect's
			// snapshot takes it away again.
			const shown = `getComputedStyle(document.querySelector('.live-off')).display !== 'none'`
			var early bool
			err = chromedp.Run(ctx,
				chromedp.Evaluate(shown, &early),
				chromedp.ActionFunc(func(context.Context) error { srv.CloseClientConnections(); return nil }),
				chromedp.Poll(shown, nil, chromedp.WithPollingTimeout(3*time.Second)),
				chromedp.Poll(`!(`+shown+`)`, nil, chromedp.WithPollingTimeout(10*time.Second)),
			)
			if err != nil || early {
				t.Fatalf("the paused banner: %v (shown while live: %v)", err, early)
			}
			if w != 390 {
				return
			}
			// Delete: the first click only opens the step; Yes deletes.
			var flash string
			err = chromedp.Run(ctx,
				chromedp.Click(`#research-history details.confirm-step > summary`, chromedp.ByQuery),
				chromedp.WaitVisible(`#research-history details[open] .confirm-box`, chromedp.ByQuery),
				chromedp.Evaluate(fitsJS, &bad),
				shoot("delete"),
				chromedp.Click(`#research-history details[open] .confirm-box button`, chromedp.ByQuery),
				chromedp.WaitVisible(`.page .banner.ok`, chromedp.ByQuery),
				chromedp.Text(`.page .banner.ok`, &flash, chromedp.ByQuery),
			)
			if err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if c := f.called(); !slices.Equal(c, []string{"delete r-1"}) || flash != "Deleted from the list." {
				t.Errorf("calls %q, flash %q", c, flash)
			}
			for _, b := range bad {
				t.Errorf("the open confirm step at 390px: %s", b)
			}
		})
	}
}
