package dashboard

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"tenant/internal/logs"
	"tenant/internal/plugins/web"
)

// F1 in a real browser, where Datastar evaluates the feed's data-init: a
// ?trace= built to close the URL string must stay a filter value, never run
// as script. The feed then shows a line with that exact trace, which proves
// the escaped stream URL still carries the filter. Skipped without Chrome.
func TestActivityTraceNotScriptInChrome(t *testing.T) {
	chrome, err := web.DetectChrome("")
	if err != nil {
		t.Skip("no Chrome, Chromium or Edge here: ", err)
	}
	const payload = "x'),(window.__xss='fired'),('"
	l := logs.NewMemory(100)
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome))
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	const shown = `document.getElementById('activity-feed').textContent.includes('after-load-marker')`
	const shown2 = `document.getElementById('activity-feed').textContent.includes('after-cr-marker')`
	var xss, xss2 string
	var marker bool
	err = chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/activity?trace="+payload),
		chromedp.WaitVisible("#activity-feed", chromedp.ByID),
		chromedp.ActionFunc(func(context.Context) error {
			l.Logf("cron", slog.LevelInfo, "after-load-marker", "trace", payload)
			return nil
		}),
		// Datastar has run the data-init once either shows.
		chromedp.Poll(`window.__xss !== undefined || `+shown, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`typeof window.__xss`, &xss),
		chromedp.Evaluate(shown, &marker),
		// A bare CR ends an SSE line, so a tool name carrying CRs could close
		// the row's patch and open a datastar-patch-signals event, which
		// Datastar evaluates. The marker after it shows the stream got past.
		chromedp.ActionFunc(func(context.Context) error {
			l.Logf("cron", slog.LevelInfo, "cr", "trace", payload,
				"tool", "t\r\revent: datastar-patch-signals\rdata: signals (window.__xss2=document.domain)\r\r")
			l.Logf("cron", slog.LevelInfo, "after-cr-marker", "trace", payload)
			return nil
		}),
		chromedp.Poll(shown2, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`typeof window.__xss2`, &xss2),
	)
	if err != nil {
		t.Fatalf("in Chrome: %v", err)
	}
	if xss != "undefined" {
		t.Errorf("?trace= ran as script: typeof window.__xss = %q", xss)
	}
	if !marker {
		t.Error("the live feed should show the line with this trace")
	}
	if xss2 != "undefined" {
		t.Errorf("a CR in a streamed row ran as script: typeof window.__xss2 = %q", xss2)
	}
}
