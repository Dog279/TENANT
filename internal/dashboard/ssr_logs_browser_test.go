package dashboard

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"tenant/internal/logs/event"
	"tenant/internal/plugins/web"
)

// The Logs page in a real browser, where Datastar runs: the page loads and
// counts, the grid fetches its next page as it scrolls, and a click on a row
// opens its detail without leaving the page. Skipped without Chrome.
// TENANT_LOGS_SCREENSHOT=<dir> keeps a screenshot of the page.
func TestLogsPageInChrome(t *testing.T) {
	chrome, err := web.DetectChrome("")
	if err != nil {
		t.Skip("no Chrome, Chromium or Edge here: ", err)
	}
	s, _, st := logsBook(t)
	var evs []event.Event
	for i := range 250 {
		evs = append(evs, event.Event{Log: "bulk", Level: event.Level(2 + i%3), Task: "Batch", Msg: "bulk " + itoa(i)})
	}
	commit(t, st, evs...)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.WindowSize(1440, 900))
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	const rows = `document.querySelectorAll('#lg-rows .lg-row').length`
	var first, paged int
	var count, msg, loc string
	var shot []byte
	err = run(ctx,
		chromedp.Navigate(srv.URL+"/logs?q=log:bulk"),
		chromedp.WaitVisible("#lg-rows .lg-row", chromedp.ByQuery),
		chromedp.Evaluate(rows, &first),
		chromedp.Poll(`document.getElementById('lg-count').textContent.includes('match')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Text("#lg-count", &count, chromedp.ByID),
		chromedp.Evaluate(`document.getElementById('lg-grid').scrollTop = 1e9`, nil),
		chromedp.Poll(rows+` > 100`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(rows, &paged),
		chromedp.Click(`#lg-rows .lg-row:nth-child(3)`, chromedp.ByQuery),
		chromedp.WaitVisible("#lg-detail .lg-msgfull", chromedp.ByQuery),
		chromedp.Text("#lg-detail .lg-msgfull", &msg, chromedp.ByQuery),
		chromedp.WaitVisible("#lg-rows .lg-row.sel", chromedp.ByQuery),
		chromedp.Location(&loc),
		chromedp.FullScreenshot(&shot, 80),
	)
	if err != nil {
		t.Fatalf("in Chrome: %v (first %d rows, then %d; count %q)", err, first, paged, count)
	}
	if first != logsPageSize || paged < 2*logsPageSize {
		t.Errorf("rows: %d, then %d after scrolling", first, paged)
	}
	if !strings.Contains(count, "250 events; 250 match") {
		t.Errorf("count = %q", count)
	}
	if msg != "bulk 247" || strings.Contains(loc, "/logs/event/") {
		t.Errorf("the detail = %q at %s: a click should open the third row's detail in place", msg, loc)
	}
	if dir := os.Getenv("TENANT_LOGS_SCREENSHOT"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "logs-page.jpg"), shot, 0o644)
	}
}

// UX-1: at phone width the Logs panes stack instead of overlapping, and the
// tree's links (Summary, Components) keep their height, with a row's detail
// open.
func TestLogsPagePhoneInChrome(t *testing.T) {
	s, _, st := logsBook(t)
	s.cfg.Addr, s.cfg.Auth = "127.0.0.1:0", GenerateToken()
	commit(t, st, event.Event{Log: "bulk", Level: event.Warning, Task: "Batch", Msg: "bulk 1"}, event.Event{Log: "bulk", Level: event.Info, Msg: "bulk 2"})
	ctx, _ := chromeSignedIn(t, s, 390, 844, "/logs?q=log%3Abulk")
	var bad []string
	if err := run(ctx,
		chromedp.WaitVisible("#lg-rows .lg-row", chromedp.ByQuery),
		chromedp.Click(`#lg-rows .lg-row`, chromedp.ByQuery),
		chromedp.WaitVisible("#lg-detail .lg-msgfull", chromedp.ByQuery),
		chromedp.Evaluate(fitsJS, &bad),
	); err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("at 390px:\n%s", strings.Join(bad, "\n"))
	}
}

// On a laptop screen with a row's detail open, the tree's links keep their
// height: the flex column used to shrink them, squashing Summary and the
// feeds to a few pixels (TEN-213 UX-1).
func TestLogsPageDesktopTreeInChrome(t *testing.T) {
	s, _, st := logsBook(t)
	s.cfg.Addr, s.cfg.Auth = "127.0.0.1:0", GenerateToken()
	for i := range 30 {
		commit(t, st, event.Event{Log: "bulk" + strconv.Itoa(i), Level: event.Info, Msg: "line"})
	}
	ctx, _ := chromeSignedIn(t, s, 1470, 830, "/logs?q=log%3Abulk0")
	var short []string
	if err := run(ctx,
		chromedp.WaitVisible("#lg-rows .lg-row", chromedp.ByQuery),
		chromedp.Click(`#lg-rows .lg-row`, chromedp.ByQuery),
		chromedp.WaitVisible("#lg-detail .lg-msgfull", chromedp.ByQuery),
		chromedp.Evaluate(`[...document.querySelectorAll('.lg-tree a')].map(a => [a.textContent.trim(), a.getBoundingClientRect().height]).filter(([, h]) => h < 12).map(([n, h]) => n + ' ' + h + 'px')`, &short),
	); err != nil {
		t.Fatal(err)
	}
	if len(short) > 0 {
		t.Errorf("tree links squashed at 1470px:\n%s", strings.Join(short, "\n"))
	}
}

// The Logs forms answer in place on a phone (DESIGN.md "Forms that don't
// reload"), signed in through the form. A new maximum size saved at the
// bottom of the page shows its toast, keeps the scroll, the page (a marker
// set before it survives) and the open Properties section, which now shows
// the new limit. A double tap on Save sends one post, though the redrawn
// section puts Save back under the finger: one change, one Security record.
// Debug tapped just after Save still posts (its own form, its own throttle).
// Clearing the log through its confirm step empties the grid and its count
// in place, and a tree group the operator closed stays closed.
// TENANT_UI_SCREENSHOT=<dir> keeps screenshots.
func TestLogsInlineFormsInChrome(t *testing.T) {
	s, _, st := logsBook(t)
	s.cfg.Addr, s.cfg.Auth = "127.0.0.1:0", GenerateToken()
	var evs []event.Event
	for i := range 40 {
		evs = append(evs, event.Event{Log: "cron", Task: "Run", Msg: "job " + itoa(i)})
	}
	commit(t, st, evs...)
	ctx, _ := chromeSignedIn(t, s, 390, 844, "/logs?q=log%3Acron")
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
			return os.WriteFile(filepath.Join(dir, "logs-390-"+name+".png"), png, 0o644)
		})
	}
	wait := chromedp.WithPollingTimeout(6 * time.Second)
	const saveBtn = `#lg-props button[value="save"]`

	var at []float64
	var props string
	var open bool
	err := run(ctx,
		chromedp.Poll(`document.getElementById('lg-count').textContent.includes('match')`, nil, wait), // Datastar is up
		chromedp.Click(`//summary[contains(., "Properties")]`, chromedp.BySearch),
		chromedp.WaitVisible(saveBtn, chromedp.ByQuery),
		chromedp.SetValue(`#lg-props input[name=max_mb]`, "5", chromedp.ByQuery),
		markPlace(saveBtn, saveBtn, &at),
		doubleTap(saveBtn),
		toastSays("ok", "Log cron changed."),
		tapsOpen,
		shot("save"),
		stayed(t, "Save", saveBtn, &at),
		chromedp.Text(`#lg-props`, &props, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('#lg-props').closest('details').open`, &open),
	)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.Contains(props, "of 5.0 MB") || !open {
		t.Errorf("the Properties section (open %v) doesn't show the new limit: %q", open, props)
	}
	if n := len(security(t, st, 4902)); n != 1 {
		t.Errorf("a double tap on Save filed %d changes, want one post", n)
	}

	// Debug as soon as the tap guard lets it through after Save, well inside
	// Save's 1s throttle: a throttle shared with Save would drop it.
	err = run(ctx,
		chromedp.Sleep(1100*time.Millisecond), // the double tap's throttle is over
		chromedp.Click(saveBtn, chromedp.ByQuery),
		tapsOpen,
		chromedp.Click(`#lg-props button[value="debug"]`, chromedp.ByQuery),
		chromedp.Poll(`!!document.querySelector('#lg-props .tag')`, nil, wait),
	)
	if err != nil {
		t.Fatalf("Debug just after Save: %v", err)
	}

	// Clear, through its confirm step: the grid and its count empty in place.
	var rows int
	var count string
	var closed bool
	err = run(ctx, tapsOpen,
		chromedp.Evaluate(`document.querySelector('#lg-tree .lg-group').open = false`, nil),
		chromedp.Click(`//summary[contains(., "Clear log")]`, chromedp.BySearch),
		markPlace(`#lg-clear`, `#lg-clear`, &at),
		chromedp.Click(`#lg-clear details.confirm-step > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(`#lg-clear details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.Click(`#lg-clear details[open] .confirm-box button`, chromedp.ByQuery),
		toastSays("ok", "Cleared 40 events from cron."),
		shot("clear"),
		chromedp.Evaluate(`document.querySelectorAll('#lg-rows .lg-row').length`, &rows),
		chromedp.Text(`#lg-count`, &count, chromedp.ByQuery),
		chromedp.Evaluate(`!document.querySelector('#lg-tree .lg-group').open`, &closed),
		stayed(t, "Clear", `#lg-clear`, &at),
	)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !closed {
		t.Error("the redrawn tree opened a group the operator closed")
	}
	if rows != 0 || !strings.HasSuffix(count, "; 0 match") {
		t.Errorf("after a clear: %d rows, count %q; want no rows and 0 match", rows, count)
	}
}

// F1 in a real browser, on the Logs page's live tail: a trace filter built to
// close the tail's URL string stays a filter value, never script, and the
// tail still shows a line with that trace. A bare CR ends an SSE line, so a
// message carrying CRs could close its row's patch and open a
// datastar-patch-signals event, which Datastar evaluates; the marker after
// it shows the stream got past it as text. (The Activity feed's test, moved
// here with the page.)
func TestLogsTailNotScriptInChrome(t *testing.T) {
	const payload = "x'),(window.__xss='fired'),('"
	s, _, st := logsBook(t)
	s.cfg.Addr, s.cfg.Auth = "127.0.0.1:0", GenerateToken()
	ctx, _ := chromeSignedIn(t, s, 390, 844, "/logs?"+url.Values{"q": {filterTerm("trace", payload)}, "live": {"1"}}.Encode())
	traced := func(msg string) event.Event {
		return event.Event{Log: "cron", Msg: msg, Corr: event.Correlation{TraceID: payload}}
	}
	const shown = `document.getElementById('lg-rows').textContent.includes('after-load-marker')`
	const shown2 = `document.getElementById('lg-rows').textContent.includes('after-cr-marker')`
	var xss, xss2 string
	var marker bool
	err := run(ctx,
		chromedp.WaitReady("#lg-tail", chromedp.ByID),
		// Datastar has run the tail's data-init once either shows. The tail
		// starts at the newest line when it subscribes, so a marker line
		// goes in each half second until one shows.
		chromedp.ActionFunc(func(ctx context.Context) error {
			for try := 1; ; try++ {
				commit(t, st, traced("after-load-marker"))
				err := chromedp.Poll(`window.__xss !== undefined || `+shown, nil, chromedp.WithPollingTimeout(500*time.Millisecond)).Do(ctx)
				if !errors.Is(err, chromedp.ErrPollingTimeout) || try == 20 {
					return err
				}
			}
		}),
		chromedp.Evaluate(`typeof window.__xss`, &xss),
		chromedp.Evaluate(shown, &marker),
		chromedp.ActionFunc(func(context.Context) error {
			cr := commit(t, st, traced("t\r\revent: datastar-patch-signals\rdata: signals (window.__xss2=document.domain)\r\r"))
			if e, err := st.Get(context.Background(), cr[0].Seq); err != nil || !strings.Contains(e.Msg, "\r") {
				t.Errorf("the store didn't keep the CRs, so this checks nothing: %q %v", e.Msg, err)
			}
			commit(t, st, traced("after-cr-marker"))
			return nil
		}),
		chromedp.Poll(shown2, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`typeof window.__xss2`, &xss2),
	)
	if err != nil {
		t.Fatalf("in Chrome: %v", err)
	}
	if xss != "undefined" {
		t.Errorf("the trace filter ran as script: typeof window.__xss = %q", xss)
	}
	if !marker {
		t.Error("the live tail should show the line with this trace")
	}
	if xss2 != "undefined" {
		t.Errorf("a CR in a streamed row ran as script: typeof window.__xss2 = %q", xss2)
	}
}
