package dashboard

import (
	"cmp"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"tenant/internal/plugins/web"
)

// fitsJS lists what doesn't fit a phone: the page, the sidebar or the menu
// scrolling sideways, or a visible control ending past the screen's edge.
// Only the inner scrollers (DESIGN.md) may be wider than the screen.
const fitsJS = `(() => {
  const bad = [];
  for (const sel of ['.page', '.side', '#nav']) {
    const e = document.querySelector(sel);
    if (e && e.scrollWidth > e.clientWidth + 1) bad.push(sel + ' is ' + e.scrollWidth + 'px wide in ' + e.clientWidth + 'px');
  }
  for (const e of document.querySelectorAll('button, a, input, select, textarea, summary')) {
    if (e.closest('.table-wrap, .lg-grid, pre, .diff, .q-detail') || e.type === 'hidden') continue;
    const r = e.getBoundingClientRect();
    if (!r.width && !r.height) continue;
    if (r.right > innerWidth + 1) bad.push(e.tagName.toLowerCase() + ' "' + (e.textContent || e.name || '').trim().slice(0, 40) + '" ends at ' + Math.round(r.right) + 'px');
  }
  return bad;
})()`

// The dashboard shell on a phone (390x844), through the production handler
// chain: sign in through the form (a Referrer-Policy or Origin regression
// breaks this first), then every page fits the screen with the populated
// fakes, the menu opens and navigates, Models' Remove goes through its
// confirm step, and the busy strip comes and goes. Skipped without Chrome.
// TENANT_UI_SCREENSHOT=<dir> keeps a screenshot of each page.
func TestUIShellInChrome(t *testing.T) {
	chrome, err := web.DetectChrome("")
	if err != nil {
		t.Skip("no Chrome, Chromium or Edge here: ", err)
	}
	tok := GenerateToken()
	runner := newBlockRunner()
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: tok}, populated, runner)
	srv := httptest.NewServer(s.served())
	defer srv.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.WindowSize(390, 844))
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	shots := os.Getenv("TENANT_UI_SCREENSHOT")
	shoot := func(name string) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			if shots == "" {
				return nil
			}
			var png []byte
			if err := chromedp.FullScreenshot(&png, 80).Do(ctx); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(shots, "ui-"+name+".jpg"), png, 0o644)
		})
	}

	// Sign in: /safety sends the visitor to the form, which brings them back.
	var signIn, back string
	var width int
	err = chromedp.Run(ctx,
		// A phone: headless windows are at least 500px wide, and touch makes
		// the 44px targets (pointer:coarse) apply.
		chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile, chromedp.EmulateTouch),
		chromedp.Navigate(srv.URL+"/safety"),
		chromedp.WaitVisible(`input[name=token]`, chromedp.ByQuery),
		chromedp.Location(&signIn),
		chromedp.SendKeys(`input[name=token]`, tok, chromedp.ByQuery),
		chromedp.Click(`form button[type=submit]`, chromedp.ByQuery),
		chromedp.WaitReady(`#ui-status`, chromedp.ByQuery),
		chromedp.Location(&back),
		chromedp.Evaluate(`innerWidth`, &width),
	)
	if err != nil {
		t.Fatalf("signing in: %v (at %s)", err, signIn)
	}
	if !strings.HasSuffix(signIn, "/settings?next=%2Fsafety") || back != srv.URL+"/safety" {
		t.Fatalf("sign-in went %s → %s, want /settings?next=%%2Fsafety → /safety", signIn, back)
	}
	if width != 390 {
		t.Fatalf("innerWidth = %d, want 390", width)
	}

	for _, p := range harnessPages {
		var bad []string
		if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+p), chromedp.WaitReady("body", chromedp.ByQuery),
			chromedp.Evaluate(fitsJS, &bad), shoot(cmp.Or(strings.Trim(strings.ReplaceAll(p, "/", "-"), "-"), "overview"))); err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		for _, b := range bad {
			t.Errorf("%s at 390px: %s", p, b)
		}
	}

	// The menu: hidden until Menu opens it, fits, ends with Log out, navigates.
	var hidden, open, logoutShown bool
	var navBad []string
	var loc string
	err = chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.WaitVisible(`.menu-btn`, chromedp.ByQuery),
		chromedp.Evaluate(`getComputedStyle(document.getElementById('nav')).display === 'none'`, &hidden),
		chromedp.Click(`.menu-btn`, chromedp.ByQuery),
		chromedp.Poll(`document.querySelector('#nav:popover-open') !== null`, &open, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Evaluate(fitsJS, &navBad),
		chromedp.Evaluate(`(() => { const b = document.querySelector('#nav .logout'); return b.getClientRects().length > 0 && b.closest('form') === document.querySelector('#nav').lastElementChild; })()`, &logoutShown),
		shoot("menu"),
		chromedp.Click(`#nav a[href="/cron"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.hdr h1`, chromedp.ByQuery),
		chromedp.Location(&loc),
	)
	if err != nil {
		t.Fatalf("the menu: %v", err)
	}
	if !hidden || !open || !logoutShown || loc != srv.URL+"/cron" {
		t.Errorf("menu: hidden at first %v, opens %v, Log out last and shown %v, navigated to %s", hidden, open, logoutShown, loc)
	}
	for _, b := range navBad {
		t.Errorf("the open menu at 390px: %s", b)
	}

	// Models' Remove: the first click only opens the step; Yes removes.
	models := s.models.(*fakeModelCtl)
	name := models.models[1].Name
	row := `#model-` + name
	var boxBad []string
	var flash string
	err = chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/models"),
		chromedp.WaitVisible(row, chromedp.ByQuery),
		chromedp.Click(row+` details.confirm-step:last-of-type > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(row+` details[open] .confirm-box`, chromedp.ByQuery),
		chromedp.Evaluate(fitsJS, &boxBad),
		shoot("models-confirm"),
		chromedp.Click(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.WaitVisible(`.page .ok`, chromedp.ByQuery),
		chromedp.Text(`.page .ok`, &flash, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Models Remove: %v", err)
	}
	if !slices.Equal(models.removeCalls, []string{name}) || !strings.Contains(flash, "removed "+name) {
		t.Errorf("Remove calls %q, flash %q; want one call for %s", models.removeCalls, flash, name)
	}
	for _, b := range boxBad {
		t.Errorf("the open confirm step at 390px: %s", b)
	}

	// The busy strip: a chat turn shows on another page, and goes within 4s
	// of the turn ending (the strip's 3s poll while busy).
	chatSend(t, s, "hello")
	awaitStart(t, runner)
	var busy string
	err = chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/tools"),
		chromedp.WaitVisible(`#ui-status .busy`, chromedp.ByQuery),
		chromedp.Text(`#ui-status .busy`, &busy, chromedp.ByQuery),
		shoot("busy"),
		chromedp.ActionFunc(func(context.Context) error { close(runner.release); return nil }),
		chromedp.Poll(`document.querySelector('#ui-status .busy') === null`, nil, chromedp.WithPollingTimeout(4*time.Second)),
	)
	if err != nil {
		t.Fatalf("the busy strip: %v (showed %q)", err, busy)
	}
	if !strings.Contains(busy, "Your agent is answering a message") || !strings.Contains(busy, "Stop") {
		t.Errorf("busy strip = %q", busy)
	}
}
