package dashboard

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// cspWatch runs in every document before its own scripts: it keeps the
// Content-Security-Policy violations the browser reports, and counts
// Datastar's fetches, which only happen once Datastar has evaluated an
// expression (it needs unsafe-eval to).
const cspWatch = `addEventListener('securitypolicyviolation', e => (window.__csp = window.__csp || []).push(e.violatedDirective + ' ' + e.blockedURI));
document.addEventListener('datastar-fetch', () => { window.__ds = (window.__ds || 0) + 1 });`

// D31 in a real browser: under the dashboard's Content-Security-Policy every
// page still works, signed in through the form (Datastar runs: each page's
// status strip fetches /ui/status), with no violation reported and no script
// error. And the policy bites: an inline script, an on*= handler and a
// javascript: link put into a page never run.
func TestCSPInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	ctx, srv := chromeSignedIn(t, s, 1280, 900, "/")
	var mu sync.Mutex
	var errs []string
	chromedp.ListenTarget(ctx, func(ev any) {
		mu.Lock()
		defer mu.Unlock()
		switch e := ev.(type) {
		case *runtime.EventExceptionThrown:
			errs = append(errs, e.ExceptionDetails.Text+" "+e.ExceptionDetails.URL)
		case *runtime.EventConsoleAPICalled:
			if e.Type == runtime.APITypeError {
				errs = append(errs, "console.error")
			}
		}
	})
	if err := run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(cspWatch).Do(ctx)
		return err
	})); err != nil {
		t.Fatal(err)
	}
	for _, p := range harnessPages {
		var csp []string
		mu.Lock()
		errs = nil
		mu.Unlock()
		err := run(ctx,
			chromedp.Navigate(srv.URL+p),
			chromedp.WaitReady(`#ui-status`, chromedp.ByQuery),
			chromedp.Poll(`window.__ds > 0`, nil, chromedp.WithPollingTimeout(5*time.Second)),
			chromedp.Sleep(100*time.Millisecond),
			chromedp.Evaluate(`window.__csp || []`, &csp),
		)
		if err != nil {
			t.Errorf("%s under the policy: Datastar didn't run: %v", p, err)
			continue
		}
		mu.Lock()
		if len(csp) > 0 || len(errs) > 0 {
			t.Errorf("%s under the policy: violations %q, script errors %q", p, csp, errs)
		}
		mu.Unlock()
	}

	var ran string
	var csp []string
	err := run(ctx,
		chromedp.Navigate(srv.URL+"/tools"),
		chromedp.WaitReady(`#ui-status`, chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
  const d = document.createElement('div');
  d.innerHTML = '<img src="/no-such-image" onerror="window.__ran = (window.__ran || \'\') + \'handler \'"><a id="jsl" href="javascript:window.__ran = (window.__ran || \'\') + \'link \'">x</a>';
  document.body.append(d);
  const sc = document.createElement('script');
  sc.textContent = "window.__ran = (window.__ran || '') + 'inline '";
  document.body.append(sc);
  document.getElementById('jsl').click();
  return true;
})()`, nil),
		chromedp.Poll(`(window.__csp || []).length >= 3`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Evaluate(`String(window.__ran || '')`, &ran),
		chromedp.Evaluate(`window.__csp`, &csp),
	)
	if err != nil {
		t.Fatalf("injected script: %v (reported %q)", err, csp)
	}
	if ran != "" {
		t.Errorf("injected script ran under the policy: %q", ran)
	}
	if !strings.Contains(strings.Join(csp, " "), "script-src") {
		t.Errorf("the violations don't name script-src: %q", csp)
	}
}
