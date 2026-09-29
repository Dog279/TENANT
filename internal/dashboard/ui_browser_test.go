package dashboard

import (
	"cmp"
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/plugins/web"
)

// fitsJS lists what doesn't fit a phone: the page, the sidebar or the menu
// scrolling sideways, a visible control ending past the screen's edge or
// squeezed too short to read, or the Logs panes drawn over each other.
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
    const name = e.tagName.toLowerCase() + ' "' + (e.textContent || e.name || '').trim().slice(0, 40) + '"';
    if (r.right > innerWidth + 1) bad.push(name + ' ends at ' + Math.round(r.right) + 'px');
    if (r.height < 12 && e.textContent.trim()) bad.push(name + ' is squeezed to ' + Math.round(r.height) + 'px tall');
  }
  for (const g of document.querySelectorAll('.lgv')) {
    const panes = [...g.children].map(k => k.getBoundingClientRect()).filter(r => r.width && r.height);
    for (let i = 0; i < panes.length; i++) for (let j = i + 1; j < panes.length; j++) {
      const a = panes[i], b = panes[j];
      if (a.left < b.right - 1 && b.left < a.right - 1 && a.top < b.bottom - 1 && b.top < a.bottom - 1) bad.push('Logs panes ' + i + ' and ' + j + ' overlap');
    }
  }
  return bad;
})()`

// tapsOpen waits out layout.html's tap guard: once a form or a button
// posts, the page takes no tap until the answer is 400ms old. A step that
// taps soon after an answer waits here first.
// It is also how a step waits out a double tap: while a post is in flight
// the guard is up, so once it is open any second post has been answered.
var tapsOpen = chromedp.Poll(`!(performance.now() < document.querySelector('.app').tapUntil)`, nil, chromedp.WithPollingTimeout(6*time.Second))

// datastarUp waits for Datastar: the status strip's first GET is back.
var datastarUp = chromedp.Poll(`performance.getEntriesByType('resource').some(e => e.name.includes('/ui/status'))`, nil, chromedp.WithPollingTimeout(6*time.Second))

// visit opens to and waits for Datastar.
func visit(to string) chromedp.Action {
	return chromedp.Tasks{chromedp.Navigate(to), datastarUp}
}

// toastSays waits for the toast of kind ok or err to say text, and for the
// answer that brought it to end (the tap guard is no longer up for it): the
// toast is its first patch, and the rest (a create's fresh region, sent
// after a flush) can land a moment later.
func toastSays(kind, text string) chromedp.Action {
	q := strconv.Quote // a Go-quoted ASCII string is a JS string literal
	return chromedp.Poll(`(t => t.classList.contains(`+q(kind)+`) && t.textContent.includes(`+q(text)+`))(document.getElementById('toast')) && document.querySelector('.app').tapUntil !== Infinity`, nil, chromedp.WithPollingTimeout(6*time.Second))
}

// tapTwice taps twice, 120ms apart, at the spot the JS expression at gives.
func tapTwice(at string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var xy []float64
		if err := chromedp.Evaluate(at, &xy).Do(ctx); err != nil {
			return err
		}
		if len(xy) != 2 {
			return errors.New("no spot to tap: " + at)
		}
		if err := chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx); err != nil {
			return err
		}
		time.Sleep(120 * time.Millisecond)
		return chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx)
	})
}

// doubleTap taps the middle of sel twice at one spot.
func doubleTap(sel string) chromedp.Action {
	return tapTwice(`(r => [r.left + r.width / 2, r.top + r.height / 2])(document.querySelector(` + strconv.Quote(sel) + `).getBoundingClientRect())`)
}

// transientCDP are the errors of a step that met a JavaScript context or a
// node just replaced, by a navigation or a morph: the same step a moment
// later works.
var transientCDP = []string{
	"Cannot find context with specified id",
	"Execution context was destroyed",
	"Inspected target navigated or closed",
	"Node with given id does not belong to the document",
	"No node with given id found",
}

// run is chromedp.Run giving each step (each one in a Tasks) up to five
// tries, 100ms apart, while it fails on a transient CDP error. A step fails
// that way in its lookups, before a tap it makes, so it taps once.
func run(ctx context.Context, actions ...chromedp.Action) error {
	return chromedp.Run(ctx, retried(actions))
}

func retried(actions chromedp.Tasks) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for _, a := range actions {
			if ts, ok := a.(chromedp.Tasks); ok {
				a = retried(ts)
			}
			for try := 1; ; try++ {
				err := a.Do(ctx)
				if err == nil {
					break
				}
				if try == 5 || !slices.ContainsFunc(transientCDP, func(s string) bool { return strings.Contains(err.Error(), s) }) {
					return err
				}
				select {
				case <-ctx.Done():
					return err
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		return nil
	})
}

// placeJS is where the page is: how far down it is scrolled (the .page
// scroller and the window), and ref's top and bottom on a screen innerHeight
// tall; null without ref.
func placeJS(ref string) string {
	return `(e => e && (r => [(document.querySelector('.page')?.scrollTop || 0) + scrollY, r.top, r.bottom, innerHeight])(e.getBoundingClientRect()))(document.querySelector(` + strconv.Quote(ref) + `))`
}

// markPlace puts sel's element in the middle of the screen, sets the marker
// a reload would lose, and notes the place (placeJS) of ref's element: the
// one tapped, or a neighbour when the answer replaces it.
func markPlace(sel, ref string, at *[]float64) chromedp.Action {
	return chromedp.Evaluate(`document.querySelector(`+strconv.Quote(sel)+`).scrollIntoView({block: 'center'}); window.inlineMark = 'kept'; `+placeJS(ref), at)
}

// stayed checks, after markPlace and an answer in place, that the page stayed
// where the operator was: the marker is there (no reload); a page scrolled
// past 100px still is (it didn't jump to the top); ref is still on the
// screen, within 60px of where it was (room for Chrome's scroll anchoring and
// a line of text changing above it); and the page fits.
func stayed(t *testing.T, step, ref string, at *[]float64) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var now []float64
		var mark string
		var bad []string
		if err := run(ctx, chromedp.Evaluate(placeJS(ref), &now), chromedp.Evaluate(`String(window.inlineMark)`, &mark), chromedp.Evaluate(fitsJS, &bad)); err != nil {
			return err
		}
		was := *at
		if mark != "kept" {
			t.Errorf("%s: the page reloaded: the marker is %q", step, mark)
		}
		switch {
		case len(was) != 4:
			t.Errorf("%s: %s wasn't on the page before", step, ref)
		case len(now) != 4:
			t.Errorf("%s: %s is gone; the page was scrolled %.0fpx", step, ref, was[0])
		case was[0] > 100 && now[0] <= 100:
			t.Errorf("%s: the page jumped to the top: scrolled %.0fpx before, %.0fpx after", step, was[0], now[0])
		case now[2] <= 0 || now[1] >= now[3]:
			t.Errorf("%s: %s left the screen: its top went from %.0fpx to %.0fpx on a %.0fpx screen (scrolled %.0fpx, then %.0fpx)", step, ref, was[1], now[1], now[3], was[0], now[0])
		case math.Abs(now[1]-was[1]) > 60:
			t.Errorf("%s: %s moved on the screen from %.0fpx to %.0fpx, over 60px (scrolled %.0fpx, then %.0fpx)", step, ref, was[1], now[1], was[0], now[0])
		}
		for _, b := range bad {
			t.Errorf("%s: %s", step, b)
		}
		return nil
	})
}

// chromeSignedIn starts Chrome at w×h and signs in to s, served through
// served(), the chain the operator's browser meets: opening page sends the
// visitor to the sign-in form, which brings them back there (a
// Referrer-Policy or Origin regression breaks this first). Narrower than
// 640px it is a phone: mobile and touch emulation, since headless windows are
// at least 500px wide and touch makes the 44px targets apply. Every browser
// test starts here (plan §6.2). It skips without Chrome; the browser and then
// the server close when the test ends.
func chromeSignedIn(t *testing.T, s *Server, w, h int, page string) (context.Context, *httptest.Server) {
	t.Helper()
	chrome, err := web.DetectChrome("")
	if err != nil {
		t.Skip("no Chrome, Chromium or Edge here: ", err)
	}
	stress := os.Getenv("TENANT_CHROME_STRESS") == "1"
	handler := s.served()
	if stress {
		// A busy runner's server answers late and out of order: each
		// request waits up to a quarter second first.
		served := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(rand.N(250 * time.Millisecond))
			served.ServeHTTP(w, r)
		})
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.WindowSize(w, h))
	var phone []chromedp.EmulateViewportOption
	if w < 640 {
		phone = append(phone, chromedp.EmulateMobile, chromedp.EmulateTouch)
	}
	var signIn, back string
	var width int
	var gaps []float64 // the sign-in card's space on the left and right
	var ctx context.Context
	// A CI runner's Chrome now and then hangs before the first page loads
	// (seen on macOS): the sign-in gets a minute, then one fresh browser.
	// An app bug fails both tries.
	for try := 1; ; try++ {
		actx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
		bctx, cancelBrowser := chromedp.NewContext(actx)
		tctx, cancelTimeout := context.WithTimeout(bctx, 3*time.Minute)
		stop := func() { cancelTimeout(); cancelBrowser(); cancelAlloc() }
		signIn, back, width, gaps = "", "", 0, nil
		// The first Run starts the browser and ties it to tctx, so the minute
		// is a timer that stops this browser, not a child context (whose end
		// would close the browser after a good sign-in too).
		done := make(chan error, 1)
		go func() {
			done <- chromedp.Run(tctx,
				chromedp.EmulateViewport(int64(w), int64(h), phone...),
				chromedp.Navigate(srv.URL+page),
				chromedp.WaitVisible(`input[name=token]`, chromedp.ByQuery),
				chromedp.Location(&signIn),
				chromedp.Evaluate(`(r => [r.left, innerWidth - r.right])(document.querySelector('form.card').getBoundingClientRect())`, &gaps),
				chromedp.SendKeys(`input[name=token]`, s.cfg.Auth, chromedp.ByQuery),
				chromedp.Click(`form button[type=submit]`, chromedp.ByQuery),
				chromedp.WaitReady(`#ui-status`, chromedp.ByQuery),
				chromedp.Location(&back),
				chromedp.Evaluate(`innerWidth`, &width),
			)
		}()
		select {
		case err = <-done:
		case <-time.After(time.Minute):
			// Kill the hung Chrome first: the browser context's cancel
			// waits for its process to exit, which only the allocator does.
			cancelAlloc()
			<-done
			err = errors.New("no sign-in within a minute")
		}
		if err == nil {
			ctx = tctx
			t.Cleanup(stop)
			break
		}
		stop()
		if try == 2 {
			t.Fatalf("signing in: %v (at %s)", err, signIn)
		}
		t.Logf("signing in: %v (at %s); starting Chrome again", err, signIn)
	}
	if next := "/settings?" + (url.Values{"next": {page}}).Encode(); !strings.HasSuffix(signIn, next) || back != srv.URL+page {
		t.Fatalf("sign-in went %s → %s, want %s → %s", signIn, back, next, page)
	}
	// A cold Chrome on the Windows runner can drop the first viewport
	// override; set it again before measuring anything.
	for try := 0; width != w && try < 20; try++ {
		time.Sleep(100 * time.Millisecond)
		if err := chromedp.Run(ctx, chromedp.EmulateViewport(int64(w), int64(h), phone...), chromedp.Evaluate(`innerWidth`, &width)); err != nil {
			t.Fatalf("setting the viewport: %v", err)
		}
	}
	if width != w {
		t.Fatalf("innerWidth = %d, want %d", width, w)
	}
	// #ui-status is there before Datastar: a tap sooner can miss its
	// handlers (a Logs row, a link, then opened its own page).
	if err := run(ctx, datastarUp); err != nil {
		t.Fatalf("waiting for Datastar: %v", err)
	}
	// TENANT_CHROME_STRESS=1 also slows the page's CPU fourfold, as a busy
	// CI runner does, to find the waits that only hold on a fast machine.
	if stress {
		if err := chromedp.Run(ctx, emulation.SetCPUThrottlingRate(4)); err != nil {
			t.Fatalf("slowing the CPU: %v", err)
		}
	}
	if len(gaps) != 2 || gaps[0] < 8 || gaps[1] < 8 {
		t.Errorf("the sign-in card at %dpx has %v px left and right, want room on both sides", w, gaps)
	}
	return ctx, srv
}

// The dashboard shell on a phone (390x844), signed in through the form: every
// page fits the screen with the populated fakes, the menu opens and
// navigates, Models' Remove goes through its confirm step, and the busy strip
// comes and goes. TENANT_UI_SCREENSHOT=<dir> keeps a screenshot of each page.
func TestUIShellInChrome(t *testing.T) {
	runner := newBlockRunner()
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, runner)
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/safety")
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

	for _, p := range harnessPages {
		var bad []string
		if err := run(ctx, chromedp.Navigate(srv.URL+p), chromedp.WaitReady("body", chromedp.ByQuery),
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
	err := run(ctx,
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
	err = run(ctx,
		chromedp.Navigate(srv.URL+"/models"),
		chromedp.WaitVisible(row, chromedp.ByQuery),
		chromedp.Click(row+` details.confirm-step:last-of-type > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(row+` details[open] .confirm-box`, chromedp.ByQuery),
		chromedp.Evaluate(fitsJS, &boxBad),
		shoot("models-confirm"),
		chromedp.Click(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.WaitVisible(`#toast.ok`, chromedp.ByQuery), // in place (formDone)
		chromedp.Text(`#toast`, &flash, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("Models Remove: %v", err)
	}
	if !slices.Equal(models.removeCalls, []string{name}) || !strings.Contains(flash, "Removed "+name) {
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
	err = run(ctx,
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

// The paused banner and a stale sign-in, in a real browser (§2.7, D30). A
// live stream keeps the banner hidden; a dropped one shows it at once. Once
// the sign-in goes stale (a new key), the next Datastar request's 401 sends
// the page to the sign-in form, rather than the tab polling on, refused, into
// the Security log.
func TestLiveBannerInChrome(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, agent.NewBroker(0), nil)
	s.SetSafety(&stripSafety{}, nil)
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/safety")
	const shown = `getComputedStyle(document.querySelector('.live-off')).display !== 'none'`
	var early bool
	var loc string
	err := run(ctx,
		chromedp.Sleep(time.Second),
		chromedp.Evaluate(shown, &early),
		chromedp.ActionFunc(func(context.Context) error { srv.CloseClientConnections(); return nil }),
		chromedp.Poll(shown, nil, chromedp.WithPollingTimeout(3*time.Second)),
	)
	if err != nil {
		t.Fatalf("the paused banner: %v", err)
	}
	if early {
		t.Error("the paused banner showed while the stream was live")
	}
	sctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	err = run(sctx,
		// The stream retries a second after the drop, now with a stale key;
		// if it beat the cookie, the strip's 20s poll is the next request.
		network.SetCookie(dashCookieName, "stale").WithURL(srv.URL).WithPath("/").WithHTTPOnly(true),
		chromedp.WaitVisible(`input[name=token]`, chromedp.ByQuery),
		chromedp.Location(&loc),
	)
	if err != nil {
		t.Fatalf("a stale sign-in: %v", err)
	}
	if !strings.HasSuffix(loc, "/settings?next=%2Fsafety") {
		t.Errorf("a stale sign-in went to %s, want the sign-in form with next=/safety", loc)
	}
}

// UX-9: on a phone the status strip stays short with two notices and a
// waiting request (each notice is one line), and its links are 44px targets.
func TestStatusStripPhoneInChrome(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, nil, nil)
	s.SetApprovals(&fakeApprovals{pending: []PendingApproval{{ID: "ap-1"}}})
	s.SetSafety(&stripSafety{perms: []control.PermissionInfo{
		{Category: "egress", Mode: "ask", Title: "Share private data", New: true},
		{Category: "mcp", Mode: "ask", Title: "Connect remote services", New: true},
		{Category: "sandbox", Mode: "ask", Title: "Leave the sandbox", New: true},
	}}, nil)
	s.SetAccess(&fakeAccess{view: AccessView{DiscordAvailable: true, RelayConfigured: true,
		DiscordPerms: []PermissionRow{{Category: "egress", New: true}, {Category: "mcp", New: true}, {Category: "sandbox", New: true}}}})
	ctx, _ := chromeSignedIn(t, s, 390, 844, "/")
	var strip float64
	var small []string
	if err := run(ctx,
		chromedp.WaitVisible("#ui-status .notice", chromedp.ByQuery),
		chromedp.Evaluate(`document.getElementById('ui-status').getBoundingClientRect().height`, &strip),
		chromedp.Evaluate(`[...document.querySelectorAll('#ui-status a')].filter(a => a.getBoundingClientRect().height < 44).map(a => a.textContent + ' ' + Math.round(a.getBoundingClientRect().height) + 'px')`, &small),
	); err != nil {
		t.Fatal(err)
	}
	t.Logf("strip %.0fpx", strip)
	if strip > 200 {
		t.Errorf("the strip takes %.0fpx of an 844px screen", strip)
	}
	if len(small) > 0 {
		t.Errorf("strip links under 44px: %q", small)
	}
}

// A small phone (360px) signs in with room on both sides of the card: the
// page's grid track used to size to the card's 360px and push it off the
// right edge. chromeSignedIn measures the gaps.
func TestSignInSmallPhoneInChrome(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, agent.NewBroker(0), nil)
	chromeSignedIn(t, s, 360, 740, "/")
}

// run's retries: a step failing on a transient CDP error runs again, one in
// a Tasks too; any other error ends the run there.
func TestRetriedSteps(t *testing.T) {
	var tries []string
	step := func(name string, errs ...error) chromedp.Action {
		return chromedp.ActionFunc(func(context.Context) error {
			tries = append(tries, name)
			if len(errs) == 0 {
				return nil
			}
			err := errs[0]
			errs = errs[1:]
			return err
		})
	}
	stale := errors.New("Cannot find context with specified id (-32000)")
	err := retried(chromedp.Tasks{step("a"), chromedp.Tasks{step("b", stale, stale)}, step("c", errors.New("boom")), step("d")}).Do(context.Background())
	if got := strings.Join(tries, " "); got != "a b b b c" || err == nil || err.Error() != "boom" {
		t.Errorf("ran %q and returned %v, want a, b three times, c, and c's error", got, err)
	}
}
