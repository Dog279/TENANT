package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestSannysoftCheck drives the REAL stealthed session against
// bot.sannysoft.com to verify the anti-detection fixes against a live
// detector. Gated behind TENANT_SANNYSOFT=1 (needs real Chrome +
// internet) so it never runs in normal `go test`.
//
//	TENANT_SANNYSOFT=1 go test ./internal/plugins/web/ -run TestSannysoftCheck -v
func TestSannysoftCheck(t *testing.T) {
	if os.Getenv("TENANT_SANNYSOFT") == "" {
		t.Skip("set TENANT_SANNYSOFT=1 to run the live bot-detection check")
	}
	sess, err := NewSession(Config{Headless: true})
	if err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(sess.tabCtx, 90*time.Second)
	defer cancel()

	// sannysoft keeps its load event open (a fingerprint subresource never
	// settles), so don't block on it: navigate under a short sub-timeout,
	// then proceed once the DOM is interactive.
	navCtx, navCancel := context.WithTimeout(ctx, 12*time.Second)
	_ = chromedp.Run(navCtx, chromedp.Navigate("https://bot.sannysoft.com"))
	navCancel()

	var webdriver, ua, langs, brands string
	var pluginsLen int
	if err := chromedp.Run(ctx,
		chromedp.WaitReady("body"),
		chromedp.Sleep(4*time.Second), // let the async checks populate
		chromedp.Evaluate(`String(navigator.webdriver)`, &webdriver),
		chromedp.Evaluate(`navigator.userAgent`, &ua),
		chromedp.Evaluate(`(navigator.languages||[]).join(",")`, &langs),
		chromedp.Evaluate(`navigator.plugins.length`, &pluginsLen),
		chromedp.Evaluate(`JSON.stringify((navigator.userAgentData&&navigator.userAgentData.brands)||[])`, &brands),
	); err != nil {
		t.Fatalf("run: %v", err)
	}

	var buf []byte
	if err := chromedp.Run(ctx, chromedp.FullScreenshot(&buf, 90)); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	out := filepath.Join(t.TempDir(), "sannysoft.png")
	if v := os.Getenv("TENANT_SANNYSOFT_OUT"); v != "" {
		out = v // optional: persist the screenshot for manual inspection
	}
	if err := os.WriteFile(out, buf, 0o644); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}

	t.Logf("navigator.webdriver = %s", webdriver)
	t.Logf("navigator.userAgent = %s", ua)
	t.Logf("navigator.languages = %s", langs)
	t.Logf("navigator.plugins.length = %d", pluginsLen)
	t.Logf("sec-ch-ua brands = %s", brands)
	t.Logf("screenshot saved -> %s", out)
}
