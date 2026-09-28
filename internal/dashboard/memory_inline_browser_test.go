package dashboard

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Memory forms answer in place (DESIGN.md "Forms that don't reload") on a
// phone, signed in through the form: on each page one change (halfway down
// the long ones) shows its toast, keeps the rows around it where they were
// and the page (a marker set before it survives), and the row shows the
// change. Facts: Remove through its step,
// then the Undo, which stays in view; and a finger's double tap at the edge
// of "Yes, remove" doesn't land on the Undo that takes its place. Recently
// removed: a double tap on the last Restore, at the bottom of the page,
// restores one fact; its row stays, so the next one's Restore doesn't come
// under the finger. Personality: an Edit, and an Add whose box is empty
// when it opens again. A fact's page: a double tap on "No longer true"
// marks it once, and doesn't mark it true again with the button that takes
// its place; the same for Remove and Restore, and the second tap on Restore
// doesn't open the editor. TENANT_UI_SCREENSHOT=<dir> keeps screenshots.
func TestMemoryInlineFormsInChrome(t *testing.T) {
	lm := newLiveMem()
	for i := range 12 {
		lm.m.addFact(int64(10+i), longName(i)+" lives at "+longURL(i), 0.7)
		lm.m.userFacts = append(lm.m.userFacts, SoulItem{ID: fmt.Sprintf("uf-%d", 10+i), Text: longName(i)})
	}
	for id := int64(30); id < 54; id++ {
		lm.m.addFact(id, fmt.Sprintf("Removed fact %d.", id), 0.5)
		lm.m.tombstoned[id] = true
	}
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, lm, nil, nil)
	// A notice in the status strip: it shows once Datastar is up.
	s.SetAccess(&permAccess{fakeAccess: fakeAccess{view: AccessView{DiscordAvailable: true, RelayConfigured: true, DiscordPerms: harnessPerms(true)}}})
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/memory/facts")

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
			return os.WriteFile(filepath.Join(dir, "memory-390-"+name+".png"), png, 0o644)
		})
	}
	q := strconv.Quote // a Go-quoted ASCII string is a JS string literal
	wait := chromedp.WithPollingTimeout(6 * time.Second)
	up := chromedp.Poll(`document.getElementById('ui-status').textContent.includes('3 new safety checks')`, nil, wait)
	open := func(page string) chromedp.Action {
		return chromedp.Tasks{chromedp.Navigate(srv.URL + page), up}
	}
	toastSays := func(text string) chromedp.Action {
		return chromedp.Poll(`(t => t.classList.contains('ok') && t.textContent.includes(`+q(text)+`))(document.getElementById('toast'))`, nil, wait)
	}
	// where is how far down the page is, and where ref, the row above the
	// one that changes, sits on the screen. A change can shift the scroll
	// (the Undo banner goes in above, and Chrome's scroll anchoring makes up
	// for it), but the rows around it must stay where the operator sees them.
	where := func(ref string) string {
		return `[document.querySelector('.page').scrollTop, document.querySelector(` + q(ref) + `).getBoundingClientRect().top]`
	}
	const mark = `String(window.inlineMark)`
	markAt := func(sel, ref string, before *[]float64) chromedp.Action {
		return chromedp.Evaluate(`document.querySelector(`+q(sel)+`).scrollIntoView({block: 'center'}); window.inlineMark = 'kept'; `+where(ref), before)
	}
	kept := func(step string, before, after []float64, marker string) {
		t.Helper()
		if len(before) != 2 || len(after) != 2 || before[0] < 100 {
			t.Fatalf("%s: the page was at %v before it and %v after, want well down it", step, before, after)
		}
		if math.Abs(after[1]-before[1]) > 1 {
			t.Errorf("%s: the row above moved on the screen: scroll and its top were %v, then %v", step, before, after)
		}
		if marker != "kept" {
			t.Errorf("%s: the page reloaded: the marker is %q", step, marker)
		}
	}
	doubleTap := func(sel string) chromedp.Action {
		var xy []float64
		return chromedp.Tasks{
			chromedp.Evaluate(`(b => (r => [r.left + r.width / 2, r.top + r.height / 2])(b.getBoundingClientRect()))(document.querySelector(`+q(sel)+`))`, &xy),
			chromedp.ActionFunc(func(ctx context.Context) error {
				if err := chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx); err != nil {
					return err
				}
				time.Sleep(120 * time.Millisecond)
				return chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx)
			}),
		}
	}
	// finger taps twice at xy, a quarter second apart, the way a finger
	// does: a touch with a radius, which Chrome moves onto a button near it
	// when there's none under its middle.
	finger := func(xy *[]float64) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			for range 2 {
				pt := []*input.TouchPoint{{X: (*xy)[0], Y: (*xy)[1], RadiusX: 20, RadiusY: 20, Force: 1}}
				if err := input.DispatchTouchEvent(input.TouchStart, pt).Do(ctx); err != nil {
					return err
				}
				time.Sleep(40 * time.Millisecond)
				if err := input.DispatchTouchEvent(input.TouchEnd, []*input.TouchPoint{}).Do(ctx); err != nil {
					return err
				}
				time.Sleep(200 * time.Millisecond)
			}
			return nil
		})
	}
	var before, after []float64
	var marker string
	var bad []string

	// Facts: Remove through its step, halfway down the list; then its Undo.
	fact := func(id int) string {
		return fmt.Sprintf(`#mem-facts-body .fact:has(input[name="fact"][value="%d"])`, id)
	}
	row := fact(15)
	var gone, undoInView, stepOpen bool
	err := chromedp.Run(ctx, up,
		markAt(row, fact(14), &before),
		chromedp.Click(row+` details.confirm-step > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.Click(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		toastSays("Fact removed."),
		shot("fact-removed"),
		chromedp.Evaluate(where(fact(14)), &after),
		chromedp.Evaluate(mark, &marker),
		chromedp.Evaluate(`document.querySelector(`+q(row)+`) === null && !document.getElementById('mem-facts-body').textContent.includes(`+q(longName(5))+`)`, &gone),
		chromedp.Evaluate(`document.querySelector('.page details[open]') !== null`, &stepOpen),
		chromedp.Evaluate(`(r => r.top >= 0 && r.bottom <= innerHeight)(document.querySelector('#mem-facts-body > .banner.undo').getBoundingClientRect())`, &undoInView),
		chromedp.Evaluate(fitsJS, &bad),
	)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	kept("Remove", before, after, marker)
	if !gone || stepOpen || !undoInView {
		t.Errorf("after Remove: the fact is gone %v, a step still open %v, the Undo on the screen %v", gone, stepOpen, undoInView)
	}
	for _, b := range bad {
		t.Errorf("Facts after Remove: %s", b)
	}
	var back bool
	err = chromedp.Run(ctx,
		chromedp.Click(`#mem-facts-body > .banner.undo button`, chromedp.ByQuery),
		toastSays("Fact restored."),
		chromedp.Evaluate(`document.querySelector(`+q(row)+`) !== null && document.querySelector('.banner.undo') === null`, &back),
		chromedp.Evaluate(mark, &marker),
	)
	if err != nil || !back || marker != "kept" {
		t.Errorf("Undo: %v; the fact is back and the banner gone %v, marker %q", err, back, marker)
	}

	// A finger's double tap near the right edge of "Yes, remove" in the
	// phone sheet: the second tap must not land on the Undo that replaces
	// the sheet.
	n := len(lm.calls())
	row = fact(16)
	var yes []float64
	err = chromedp.Run(ctx,
		chromedp.Click(row+` details.confirm-step > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.Evaluate(`(r => [r.right - 6, r.bottom - 10])(document.querySelector(`+q(row+` details[open] .confirm-box button`)+`).getBoundingClientRect())`, &yes),
		finger(&yes),
		toastSays("Fact removed."),
		chromedp.Sleep(300*time.Millisecond),
	)
	if got := lm.calls()[n:]; err != nil || fmt.Sprint(got) != "[delete 16]" {
		t.Errorf("a double tap at the edge of Yes, remove: %v; sent %v, want only the delete", err, got)
	}
	_ = lm.RestoreFact(16)

	// Recently removed: a double tap on the last Restore, at the bottom of
	// the page.
	n = len(lm.calls())
	var left int
	var says string
	err = chromedp.Run(ctx, open("/memory/facts/removed"),
		markAt("#removed-53", "#removed-52", &before),
		doubleTap("#removed-53 button"),
		toastSays("Fact restored."),
		chromedp.Sleep(300*time.Millisecond), // room for a second post to land
		shot("restored"),
		chromedp.Evaluate(where("#removed-52"), &after),
		chromedp.Evaluate(mark, &marker),
		chromedp.Text("#removed-53 .fact-actions", &says, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('#mem-removed-body form[action="/memory/facts/restore"]').length`, &left),
	)
	if err != nil {
		t.Fatalf("Restore: %v; sent %v", err, lm.calls()[n:])
	}
	kept("Restore", before, after, marker)
	if got := lm.calls()[n:]; fmt.Sprint(got) != "[restore 53]" || left != 23 || says != "Restored." {
		t.Errorf("a double tap on the last Restore sent %v, left %d Restore buttons, and the row says %q; want one restore, 23 and Restored.", got, left, says)
	}

	// Personality: Edit a line about you halfway down, then Add an
	// instruction.
	line := func(id string) string { return `#mem-soul-body .soul-row:has(input[name="item"][value="` + id + `"])` }
	var text string
	var empty bool
	err = chromedp.Run(ctx, open("/memory/soul"),
		markAt(line("uf-15"), line("uf-14"), &before),
		chromedp.Click(line("uf-15")+` details.go > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(line("uf-15")+` details[open] textarea`, chromedp.ByQuery),
		shot("soul-edit"),
		chromedp.SetValue(line("uf-15")+` details[open] textarea`, "Prefers short answers.", chromedp.ByQuery),
		chromedp.Click(line("uf-15")+` details[open] button[type=submit]`, chromedp.ByQuery),
		toastSays("Saved."),
		chromedp.Evaluate(where(line("uf-14")), &after),
		chromedp.Evaluate(mark, &marker),
		chromedp.Text(line("uf-15")+` .soul-text`, &text, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('.page details[open]') !== null`, &stepOpen),
		chromedp.Evaluate(fitsJS, &bad),
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	kept("Edit", before, after, marker)
	if text != "Prefers short answers." || stepOpen {
		t.Errorf("after Edit: the line says %q, a step still open %v", text, stepOpen)
	}
	for _, b := range bad {
		t.Errorf("Personality after Edit: %s", b)
	}
	add := `#mem-soul-body .card:has(input[value="instruction"]) .ghead2 details`
	err = chromedp.Run(ctx,
		chromedp.Click(add+` > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(add+`[open] textarea`, chromedp.ByQuery),
		chromedp.SetValue(add+`[open] textarea`, "Answer in German.", chromedp.ByQuery),
		chromedp.Click(add+`[open] button[type=submit]`, chromedp.ByQuery),
		toastSays("Added."),
		chromedp.Evaluate(`document.getElementById('mem-soul-body').textContent.includes('Answer in German.')`, &back),
		chromedp.Click(add+` > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(add+`[open] textarea`, chromedp.ByQuery),
		shot("soul-add-again"),
		chromedp.Evaluate(`document.querySelector(`+q(add+`[open] textarea`)+`).value === ''`, &empty),
		chromedp.Evaluate(mark, &marker),
	)
	if err != nil || !back || !empty || marker != "kept" {
		t.Errorf("Add: %v; the line shows %v, the box is empty when it opens again %v, marker %q", err, back, empty, marker)
	}

	// A fact's page: a double tap on "No longer true". The button that
	// takes its place, "It's true again", is the same form.
	n = len(lm.calls())
	var again bool
	err = chromedp.Run(ctx, open("/memory/facts/1"),
		chromedp.Evaluate(`window.inlineMark = 'kept'`, nil),
		doubleTap(`form[action="/memory/facts/valid"] button`),
		toastSays("Marked no longer true."),
		chromedp.Sleep(300*time.Millisecond),
		shot("fact-invalidated"),
		chromedp.Evaluate(mark, &marker),
		chromedp.Evaluate(`document.querySelector('form[action="/memory/facts/valid"] input[name="valid"][value="true"]') !== null && document.getElementById('mem-fact-body').textContent.includes('No longer true since')`, &again),
		chromedp.Evaluate(fitsJS, &bad),
	)
	if err != nil {
		t.Fatalf("No longer true: %v", err)
	}
	if got := lm.calls()[n:]; fmt.Sprint(got) != "[invalidate 1]" || !again || marker != "kept" {
		t.Errorf("a double tap on No longer true sent %v; the page offers It's true again %v; marker %q", got, again, marker)
	}
	for _, b := range bad {
		t.Errorf("the fact's page: %s", b)
	}

	// Then a double tap on Remove, and a finger's double tap near the right
	// edge of Restore, which takes Remove's place as the same form. The
	// second tap on Restore doesn't open the editor.
	n = len(lm.calls())
	removal := `form[action="/memory/facts/removal"] button`
	var edge []float64
	var loc string
	err = chromedp.Run(ctx,
		doubleTap(removal),
		toastSays("Fact removed."),
		chromedp.Sleep(1200*time.Millisecond), // past the form's throttle
		chromedp.Evaluate(`(r => [r.right - 4, r.top + r.height / 2])(document.querySelector(`+q(removal)+`).getBoundingClientRect())`, &edge),
		finger(&edge),
		toastSays("Fact restored."),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Location(&loc),
		chromedp.Evaluate(mark, &marker),
	)
	if got := lm.calls()[n:]; err != nil || fmt.Sprint(got) != "[delete 1 restore 1]" || loc != srv.URL+"/memory/facts/1" || marker != "kept" {
		t.Errorf("double taps on Remove, then Restore: %v; sent %v, want one of each; at %s, marker %q", err, got, loc, marker)
	}
}
