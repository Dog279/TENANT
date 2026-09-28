package dashboard

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Access forms answer in place (DESIGN.md "Forms that don't reload"), on a
// phone and on a desktop, signed in through the form. A change far down the
// page shows its toast where the screen puts it, keeps the scroll and the
// page (a marker set before it survives), and the row and the status strip
// show the new setting; the success toast fades. After a confirm step's Yes
// the step closes. A failure is an alert that stays until closed. A double
// tap on Remove removes one person, though the second tap lands on the same
// form holding the next one. A post refused for a stale page shows
// #toast-fail, and the next post hides it. The page fits throughout.
// TENANT_UI_SCREENSHOT=<dir> keeps screenshots.
func TestAccessInlineFormsInChrome(t *testing.T) {
	for _, size := range [][2]int{{390, 844}, {1280, 800}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			fa := &permAccess{fakeAccess: fakeAccess{view: AccessView{
				IMessageAvailable: true, IMessagePerms: harnessPerms(false), Handles: []string{"+15551230001", "+15551230002", "+15551230003"},
				DiscordAvailable: true, RelayConfigured: true, RelayRunning: true, OperatorSet: true, OperatorID: "4242", ExecOn: true,
				DiscordPerms: harnessPerms(true), // egress, mcp and sandbox are New and ask
			}}}
			s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, nil, nil)
			s.SetAccess(fa)
			ctx, _ := chromeSignedIn(t, s, size[0], size[1], "/access")
			phone := size[0] < 640
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
					return os.WriteFile(filepath.Join(dir, fmt.Sprintf("access-%d-%s.png", size[0], name)), png, 0o644)
				})
			}
			row := func(cat string) string {
				return `.perm:has(form[action="/access/discord/permission"] input[value="` + cat + `"])`
			}
			q := strconv.Quote // a Go-quoted ASCII string is a JS string literal
			wait := chromedp.WithPollingTimeout(6 * time.Second)
			toastSays := func(kind, text string) chromedp.Action { // kind is ok or err
				return chromedp.Poll(`(t => t.classList.contains(`+q(kind)+`) && t.textContent.includes(`+q(text)+`))(document.getElementById('toast'))`, nil, wait)
			}
			const where = `[document.querySelector('.page').scrollTop, scrollY]`
			const mark = `String(window.inlineMark)`

			// One click far down the page: Block on the last Discord row.
			var before, after, place []float64
			var kept, sandbox, strip string
			var bad []string
			err := chromedp.Run(ctx,
				chromedp.Poll(`document.getElementById('ui-status').textContent.includes('3 new safety checks')`, nil, wait), // Datastar is up
				chromedp.Evaluate(`document.querySelector(`+q(row("sandbox"))+`).scrollIntoView({block: 'center'}); window.inlineMark = 'kept'; `+where, &before),
				chromedp.Click(row("sandbox")+` form:has(input[value="deny"]) button`, chromedp.ByQuery),
				toastSays("ok", "Discord conversations: Leave the sandbox is blocked."),
				shot("block"),
				chromedp.Evaluate(where, &after),
				chromedp.Evaluate(mark, &kept),
				chromedp.Text(row("sandbox"), &sandbox, chromedp.ByQuery),
				chromedp.Text(`#ui-status`, &strip, chromedp.ByQuery),
				chromedp.Evaluate(`(r => [r.left, innerWidth - r.right, r.top, innerHeight - r.bottom])(document.getElementById('toast').getBoundingClientRect())`, &place),
				chromedp.Evaluate(fitsJS, &bad),
				chromedp.Poll(`getComputedStyle(document.getElementById('toast')).visibility === 'hidden'`, nil, wait), // it fades
			)
			if err != nil {
				t.Fatalf("Block: %v", err)
			}
			if len(before) != 2 || before[0] < 100 {
				t.Fatalf("the page scrolled to %v before the click, want well down it", before)
			}
			if fmt.Sprint(after) != fmt.Sprint(before) {
				t.Errorf("the scroll moved from %v to %v", before, after)
			}
			if kept != "kept" {
				t.Errorf("the page reloaded: the marker is %q", kept)
			}
			if !strings.Contains(sandbox, "Can't run a command outside the safety box") || strings.Contains(sandbox, "New") {
				t.Errorf("the row doesn't show its new setting: %q", sandbox)
			}
			if !strings.Contains(strip, "2 new safety checks") {
				t.Errorf("the status strip wasn't updated: %q", strip)
			}
			for _, b := range bad {
				t.Errorf("with the toast at %dpx: %s", size[0], b)
			}
			if len(place) != 4 || slices.Min(place) < 0 {
				t.Fatalf("the toast isn't all on the screen: left, right, top, bottom gaps %v", place)
			}
			if phone && (math.Abs(place[0]-place[1]) > 2 || place[2] > 40) {
				t.Errorf("on a phone the toast is across the top; gaps left, right, top, bottom are %v", place)
			}
			if !phone && (place[1] > 40 || place[2] > 40) {
				t.Errorf("on a desktop the toast is top right; gaps left, right, top, bottom are %v", place)
			}

			// A confirm step: Allow opens it, Yes closes it and says so.
			var open bool
			var mcp string
			err = chromedp.Run(ctx,
				chromedp.Click(row("mcp")+` details.confirm-step > summary`, chromedp.ByQuery),
				chromedp.WaitVisible(row("mcp")+` details[open] .confirm-box button`, chromedp.ByQuery),
				shot("allow-step"),
				chromedp.Click(row("mcp")+` details[open] .confirm-box button`, chromedp.ByQuery),
				toastSays("ok", "Discord conversations: Connect remote services is allowed without asking."),
				chromedp.Evaluate(`document.querySelector('.page details[open]') !== null`, &open),
				chromedp.Text(row("mcp"), &mcp, chromedp.ByQuery),
				chromedp.Evaluate(mark, &kept),
				chromedp.Click(`#toast .toast-x`, chromedp.ByQuery),
				chromedp.Poll(`document.getElementById('toast').getClientRects().length === 0`, nil, wait),
			)
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if open || kept != "kept" || !strings.Contains(mcp, "Can connect new remote services and trust their tools without asking you.") {
				t.Errorf("after Yes: a step still open %v, marker %q, row %q", open, kept, mcp)
			}

			// A failure: an alert that doesn't fade, and the row as it was.
			var anim, egress string
			err = chromedp.Run(ctx,
				chromedp.ActionFunc(func(context.Context) error { fa.setFail(true); return nil }),
				chromedp.Click(row("egress")+` form:has(input[value="deny"]) button`, chromedp.ByQuery),
				toastSays("err", "Couldn't change that safety setting."),
				shot("error"),
				chromedp.Evaluate(`getComputedStyle(document.getElementById('toast')).animationName`, &anim),
				chromedp.Text(row("egress"), &egress, chromedp.ByQuery),
				chromedp.Evaluate(fitsJS, &bad),
				chromedp.Click(`#toast .toast-x`, chromedp.ByQuery),
				chromedp.Poll(`document.getElementById('toast').getClientRects().length === 0`, nil, wait),
				chromedp.Evaluate(mark, &kept),
			)
			if err != nil {
				t.Fatalf("a failure: %v", err)
			}
			if anim != "none" || kept != "kept" || !strings.Contains(egress, "Asks you before it can send private information") {
				t.Errorf("the alert: animation %q (want none), marker %q, row %q", anim, kept, egress)
			}
			for _, b := range bad {
				t.Errorf("with the alert at %dpx: %s", size[0], b)
			}

			// A double tap on the first Remove, 120ms apart at one spot.
			var xy []float64
			var left int
			err = chromedp.Run(ctx,
				chromedp.Evaluate(`(b => (b.scrollIntoView({block: 'center'}), (r => [r.left + r.width / 2, r.top + r.height / 2])(b.getBoundingClientRect())))(document.querySelector('form[action="/access/imessage/deny"] button'))`, &xy),
				chromedp.ActionFunc(func(ctx context.Context) error {
					if err := chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx); err != nil {
						return err
					}
					time.Sleep(120 * time.Millisecond)
					return chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx)
				}),
				toastSays("ok", "+15551230001 can no longer message your agent."),
				chromedp.Sleep(300*time.Millisecond), // room for a second post to land
				chromedp.Evaluate(`document.querySelectorAll('form[action="/access/imessage/deny"]').length`, &left),
			)
			if err != nil {
				t.Fatalf("a double tap: %v", err)
			}
			fa.mu.Lock()
			denied := slices.Clone(fa.denied)
			fa.mu.Unlock()
			if fmt.Sprint(denied) != "[+15551230001]" || left != 2 {
				t.Errorf("a double tap on Remove sent %v and left %d Remove buttons, want one post and two", denied, left)
			}

			// A stale page: the list went down to one person elsewhere, so
			// Remove now needs its confirm step and the server says 428.
			fa.mu.Lock()
			fa.view.SingleHandle = true
			fa.mu.Unlock()
			fa.setFail(false)
			const failShown = `getComputedStyle(document.getElementById('toast-fail')).display !== 'none'`
			var shown bool
			err = chromedp.Run(ctx,
				chromedp.Click(`form:has(input[value="+15551230003"]) button`, chromedp.ByQuery),
				chromedp.Poll(failShown, nil, wait),
				shot("stale"),
				chromedp.Evaluate(`document.querySelectorAll('form[action="/access/imessage/deny"]').length`, &left),
				chromedp.Evaluate(fitsJS, &bad),
				chromedp.Click(row("sandbox")+` form:has(input[value="ask"]) button`, chromedp.ByQuery), // a form not used in the last second
				toastSays("ok", "Discord conversations: Leave the sandbox asks you first."),
				chromedp.Evaluate(failShown, &shown),
				chromedp.Evaluate(mark, &kept),
			)
			if err != nil {
				t.Fatalf("a stale page: %v", err)
			}
			if left != 2 || shown || kept != "kept" {
				t.Errorf("a stale page: %d Remove buttons (want 2), #toast-fail still shown %v, marker %q", left, shown, kept)
			}
			for _, b := range bad {
				t.Errorf("with #toast-fail at %dpx: %s", size[0], b)
			}
		})
	}
}
