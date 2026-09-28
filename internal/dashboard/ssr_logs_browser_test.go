package dashboard

import (
	"context"
	"net/http/httptest"
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
	err = chromedp.Run(ctx,
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
	if err := chromedp.Run(ctx,
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
	if err := chromedp.Run(ctx,
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
