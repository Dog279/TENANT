package dashboard

import (
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// Removing a helper on a phone (390x844), signed in through the form: the
// first tap only opens the confirm step, which fits the screen; Yes removes
// the helper and says so. The edit panel fits too.
func TestAgentsRemoveInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	fa := s.agents.(*fakeAgents)
	name := fa.rows[2].Name // the operator's own helper
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/agents")
	row := `#agent-` + name
	var boxBad, editBad []string
	var before, flash string
	err := chromedp.Run(ctx,
		chromedp.WaitVisible(row, chromedp.ByQuery),
		chromedp.Click(row+` details.confirm-step > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(row+` details[open] .confirm-box`, chromedp.ByQuery),
		chromedp.Text(row+` details[open] .confirm-box p`, &before, chromedp.ByQuery),
		chromedp.Evaluate(fitsJS, &boxBad),
		chromedp.Click(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.WaitVisible(`.page .banner.ok`, chromedp.ByQuery),
		chromedp.Text(`.page .banner.ok`, &flash, chromedp.ByQuery),
		chromedp.Navigate(srv.URL+"/agents?edit="+name),
		chromedp.WaitVisible(`#edit`, chromedp.ByQuery),
		chromedp.Click(`#edit details.advanced > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(`#edit textarea[name=soul]`, chromedp.ByQuery),
		chromedp.Evaluate(fitsJS, &editBad),
	)
	if err != nil {
		t.Fatalf("removing a helper: %v", err)
	}
	if !strings.HasPrefix(before, "Remove "+name+"?") {
		t.Errorf("the confirm step says %q", before)
	}
	if !slices.Equal(fa.changes(), []string{"Remove|" + name}) || flash != "Removed "+name+"." {
		t.Errorf("calls %q, flash %q; want one Remove of %s", fa.changes(), flash, name)
	}
	for _, b := range append(boxBad, editBad...) {
		t.Errorf("at 390px: %s", b)
	}
}
