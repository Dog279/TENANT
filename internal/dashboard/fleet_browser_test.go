package dashboard

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The Fleet page in a real browser on a phone (390x844), on the computer
// running Tenant: the list and every kind of machine page fit the screen
// with the longest names and reported values; the confirm form takes a choice
// and the six digits and answers in place, wrong digits first; a setting
// changes in place, and Copy again goes through its confirm step.
func TestFleetPageInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	f := s.fleet.(*fakeFleet)
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/fleet")

	for _, p := range []string{"/fleet", "/fleet/" + fleetPending, "/fleet/" + fleetConfirmed, "/fleet/m0000000000000003", "/fleet/m0000000000000004"} {
		var bad []string
		if err := run(ctx, chromedp.Navigate(srv.URL+p), chromedp.WaitReady("#fleet-body", chromedp.ByQuery), chromedp.Evaluate(fitsJS, &bad)); err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		for _, b := range bad {
			t.Errorf("%s at 390px: %s", p, b)
		}
	}

	// Confirm: this browser is on the computer running Tenant, so it has the
	// form. Wrong digits leave the page, the choice and the box as they are.
	form := `form[action="/fleet/confirm"]`
	var at []float64
	err := run(ctx,
		visit(srv.URL+"/fleet/"+fleetPending),
		chromedp.WaitVisible(form+` input[name=code]`, chromedp.ByQuery),
		chromedp.SetValue(form+` select[aria-label="Run commands"]`, "exec=allow", chromedp.ByQuery),
		chromedp.SendKeys(form+` input[name=code]`, "000000", chromedp.ByQuery),
		markPlace(form+` button[type=submit]`, form+` input[name=code]`, &at),
		chromedp.Click(form+` button[type=submit]`, chromedp.ByQuery),
		toastSays("err", "aren't the six digits"),
		stayed(t, "wrong digits", form+` input[name=code]`, &at),
		tapsOpen,
		chromedp.Sleep(1100*time.Millisecond), // the form's own throttle: one post a second
		chromedp.SetValue(form+` input[name=code]`, fleetCode, chromedp.ByQuery),
		chromedp.Click(form+` button[type=submit]`, chromedp.ByQuery),
		toastSays("ok", "is confirmed"),
		chromedp.WaitNotPresent(form, chromedp.ByQuery), // the region is the confirmed machine's page now
		chromedp.WaitVisible(`form[action="/fleet/permission"]`, chromedp.ByQuery),
	)
	if err != nil {
		var state string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`location.href + ' | toast: ' + document.getElementById('toast').outerHTML + ' | guard: ' + document.querySelector('.app').tapUntil + ' | flash: ' + (document.querySelector('.banner')?.outerHTML || '') + ' | code: ' + (document.querySelector('input[name=code]')?.value ?? 'gone')`, &state))
		t.Fatalf("confirming: %v (the control got %q)\n%s", err, f.called(), state)
	}
	if want := []string{"confirm|" + fleetPending + "|000000|exec=allow", "confirm|" + fleetPending + "|" + fleetCode + "|exec=allow"}; !slices.Equal(f.called(), want) {
		t.Fatalf("the control got %q, want %q", f.called(), want)
	}

	// A setting in place, then Copy again through its step.
	block := `form[action="/fleet/permission"]:has(input[name=mode][value=deny]) button`
	again := `details.confirm-step:has(input[name=from][value=master])`
	var bad []string
	err = run(ctx,
		visit(srv.URL+"/fleet/"+fleetConfirmed),
		chromedp.WaitVisible(block, chromedp.ByQuery),
		markPlace(block, `#fleet-body .perm-text strong`, &at),
		chromedp.Click(block, chromedp.ByQuery),
		toastSays("ok", "is blocked."),
		stayed(t, "Block", `#fleet-body .perm-text strong`, &at),
		tapsOpen,
		chromedp.ScrollIntoView(again, chromedp.ByQuery),
		chromedp.Click(again+` > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(again+`[open] .confirm-box`, chromedp.ByQuery),
		chromedp.Evaluate(fitsJS, &bad),
		chromedp.Click(again+`[open] .confirm-box button`, chromedp.ByQuery),
		toastSays("ok", "has a fresh copy"),
	)
	if err != nil {
		t.Fatalf("changing a setting and copying again: %v (the control got %q)", err, f.called())
	}
	for _, b := range bad {
		t.Errorf("the open Copy again step at 390px: %s", b)
	}
	if got := f.called(); len(got) != 4 || got[2] != "permission|"+fleetConfirmed+"|exec|deny" || got[3] != "recopy|"+fleetConfirmed+"|master" {
		t.Errorf("the control got %q", got)
	}

	// Add machine (TEN-541): the form answers in place with the lines, which
	// fit the phone, and the code is in no address the browser keeps.
	add := `form[action="/fleet/add"]`
	var paste, href string
	bad = nil
	err = run(ctx,
		visit(srv.URL+"/fleet"),
		chromedp.WaitVisible(add+` input[name=name]`, chromedp.ByQuery),
		chromedp.SendKeys(add+` input[name=name]`, "web9", chromedp.ByQuery),
		chromedp.Click(add+` input[name=runs][value=boot]`, chromedp.ByQuery),
		chromedp.Click(add+` button[type=submit]`, chromedp.ByQuery),
		toastSays("ok", "shown this once"),
		chromedp.WaitVisible(`#fleet-body code.fl-paste`, chromedp.ByQuery),
		chromedp.Text(`#fleet-body code.fl-paste`, &paste, chromedp.ByQuery),
		chromedp.Evaluate(`location.href`, &href),
		chromedp.Evaluate(fitsJS, &bad),
	)
	if err != nil {
		t.Fatalf("Add machine: %v (the control got %q)", err, f.called())
	}
	if !strings.Contains(paste, fakeEnrollCode) || strings.Contains(href, fakeEnrollCode) {
		t.Errorf("Add machine: the line %q, the address %q", paste, href)
	}
	for _, b := range bad {
		t.Errorf("Add machine's answer at 390px: %s", b)
	}
	if got := f.called(); len(got) != 5 || !strings.HasPrefix(got[4], "enroll|web9|true|dashboard session ") {
		t.Errorf("the control got %q", got)
	}
}
