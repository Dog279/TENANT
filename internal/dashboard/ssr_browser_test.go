package dashboard

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// assetEntriesJS is what the page's Performance API saw of the stylesheet
// and Datastar: each one's URL and the bytes it took from the network (0
// when the browser's cache answered).
const assetEntriesJS = `performance.getEntriesByType('resource')
  .filter(e => /\/(styles\.css|datastar\.js)/.test(new URL(e.name).pathname))
  .map(e => ({url: e.name, transfer: e.transferSize}))`

// The second page a browser opens takes the stylesheet and Datastar from its
// cache: they are linked by content hash and immutable, so not a byte of
// them crosses the network again.
func TestAssetsCachedInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	ctx, srv := chromeSignedIn(t, s, 1280, 900, "/")
	type entry struct {
		URL      string `json:"url"`
		Transfer int    `json:"transfer"`
	}
	var first, second []entry
	var doc int
	err := run(ctx,
		chromedp.Poll(`document.readyState === 'complete'`, nil),
		chromedp.Evaluate(assetEntriesJS, &first),
		chromedp.Navigate(srv.URL+"/tools"),
		chromedp.WaitReady(`#ui-status`, chromedp.ByQuery),
		chromedp.Poll(`document.readyState === 'complete'`, nil),
		chromedp.Evaluate(assetEntriesJS, &second),
		chromedp.Evaluate(`performance.getEntriesByType('navigation')[0].transferSize`, &doc),
	)
	if err != nil {
		t.Fatal(err)
	}
	sum := func(es []entry) (n int) {
		for _, e := range es {
			n += e.Transfer
		}
		return n
	}
	t.Logf("assets over the network: first page %d bytes %v, second page %d bytes %v (its HTML %d bytes)", sum(first), first, sum(second), second, doc)
	if len(second) != 2 {
		t.Fatalf("the second page loaded %v, want the stylesheet and Datastar once each", second)
	}
	for _, e := range second {
		if !strings.Contains(e.URL, "?v=") || e.Transfer != 0 {
			t.Errorf("second page: %s took %d bytes from the network, want a hashed URL from the cache (0)", e.URL, e.Transfer)
		}
	}
}
