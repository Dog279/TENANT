package dashboard

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// Autopilot on a phone (390x844), signed in through the form: a running goal
// at its widest fits, and one tap on Stop (no confirm step) stops it in
// place: the idle state shows, with a toast.
func TestGoalStopInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	ctx, _ := chromeSignedIn(t, s, 390, 844, "/goal")
	g := s.goal.(*fakeGoal)
	var before, after []string
	var flash string
	err := run(ctx,
		chromedp.WaitVisible(`.page .busy`, chromedp.ByQuery),
		datastarUp,
		chromedp.Evaluate(fitsJS, &before),
		chromedp.Click(`form[action="/goal/stop"] button`, chromedp.ByQuery),
		chromedp.WaitVisible(`#toast.ok`, chromedp.ByQuery),
		chromedp.Text(`#toast .toast-text`, &flash, chromedp.ByQuery),
		chromedp.WaitVisible(`.page .empty`, chromedp.ByQuery),
		chromedp.Evaluate(fitsJS, &after),
	)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if g.clearCalls() != 1 || !strings.HasPrefix(flash, "Stopped.") {
		t.Errorf("Clear calls %d, flash %q; want one call and Stopped", g.clearCalls(), flash)
	}
	for _, b := range append(before, after...) {
		t.Errorf("/goal at 390px: %s", b)
	}
}
