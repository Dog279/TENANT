package dashboard

import (
	"context"
	"fmt"
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
	var at []float64

	// Facts: Remove through its step, halfway down the list; then its Undo.
	fact := func(id int) string {
		return fmt.Sprintf(`#mem-facts-body .fact:has(input[name="fact"][value="%d"])`, id)
	}
	row := fact(15)
	var gone, undoInView, stepOpen bool
	err := run(ctx,
		markPlace(row, fact(14), &at),
		chromedp.Click(row+` details.confirm-step > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.Click(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		toastSays("ok", "Fact removed."),
		shot("fact-removed"),
		stayed(t, "Remove", fact(14), &at),
		chromedp.Evaluate(`document.querySelector(`+q(row)+`) === null && !document.getElementById('mem-facts-body').textContent.includes(`+q(longName(5))+`)`, &gone),
		chromedp.Evaluate(`document.querySelector('.page details[open]') !== null`, &stepOpen),
		chromedp.Evaluate(`(r => r.top >= 0 && r.bottom <= innerHeight)(document.querySelector('#mem-facts-body > .banner.undo').getBoundingClientRect())`, &undoInView),
	)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !gone || stepOpen || !undoInView {
		t.Errorf("after Remove: the fact is gone %v, a step still open %v, the Undo on the screen %v", gone, stepOpen, undoInView)
	}
	var back bool
	err = run(ctx, tapsOpen,
		chromedp.Click(`#mem-facts-body > .banner.undo button`, chromedp.ByQuery),
		toastSays("ok", "Fact restored."),
		chromedp.Evaluate(`document.querySelector(`+q(row)+`) !== null && document.querySelector('.banner.undo') === null`, &back),
		stayed(t, "Undo", fact(14), &at),
	)
	if err != nil || !back {
		t.Errorf("Undo: %v; the fact is back and the banner gone %v", err, back)
	}

	// A finger's double tap near the right edge of "Yes, remove" in the
	// phone sheet: the second tap must not land on the Undo that replaces
	// the sheet.
	n := len(lm.calls())
	row = fact(16)
	var yes []float64
	err = run(ctx, tapsOpen,
		chromedp.Click(row+` details.confirm-step > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(row+` details[open] .confirm-box button`, chromedp.ByQuery),
		chromedp.Evaluate(`(r => [r.right - 6, r.bottom - 10])(document.querySelector(`+q(row+` details[open] .confirm-box button`)+`).getBoundingClientRect())`, &yes),
		finger(&yes),
		toastSays("ok", "Fact removed."),
		tapsOpen,
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
	err = run(ctx, visit(srv.URL+"/memory/facts/removed"),
		markPlace("#removed-53", "#removed-52", &at),
		doubleTap("#removed-53 button"),
		toastSays("ok", "Fact restored."),
		tapsOpen,
		shot("restored"),
		stayed(t, "Restore", "#removed-52", &at),
		chromedp.Text("#removed-53 .fact-actions", &says, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('#mem-removed-body form[action="/memory/facts/restore"]').length`, &left),
	)
	if err != nil {
		t.Fatalf("Restore: %v; sent %v", err, lm.calls()[n:])
	}
	if got := lm.calls()[n:]; fmt.Sprint(got) != "[restore 53]" || left != 23 || says != "Restored." {
		t.Errorf("a double tap on the last Restore sent %v, left %d Restore buttons, and the row says %q; want one restore, 23 and Restored.", got, left, says)
	}

	// Personality: Edit a line about you halfway down, then Add an
	// instruction.
	line := func(id string) string { return `#mem-soul-body .soul-row:has(input[name="item"][value="` + id + `"])` }
	var text string
	var empty bool
	err = run(ctx, visit(srv.URL+"/memory/soul"),
		markPlace(line("uf-15"), line("uf-14"), &at),
		chromedp.Click(line("uf-15")+` details.go > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(line("uf-15")+` details[open] textarea`, chromedp.ByQuery),
		shot("soul-edit"),
		chromedp.SetValue(line("uf-15")+` details[open] textarea`, "Prefers short answers.", chromedp.ByQuery),
		chromedp.Click(line("uf-15")+` details[open] button[type=submit]`, chromedp.ByQuery),
		toastSays("ok", "Saved."),
		stayed(t, "Edit", line("uf-14"), &at),
		chromedp.Text(line("uf-15")+` .soul-text`, &text, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('.page details[open]') !== null`, &stepOpen),
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if text != "Prefers short answers." || stepOpen {
		t.Errorf("after Edit: the line says %q, a step still open %v", text, stepOpen)
	}
	add := `#mem-soul-body .card:has(input[value="instruction"]) .ghead2 details`
	err = run(ctx, tapsOpen,
		markPlace(add, add, &at),
		chromedp.Click(add+` > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(add+`[open] textarea`, chromedp.ByQuery),
		chromedp.SetValue(add+`[open] textarea`, "Answer in German.", chromedp.ByQuery),
		chromedp.Click(add+`[open] button[type=submit]`, chromedp.ByQuery),
		toastSays("ok", "Added."),
		chromedp.Evaluate(`document.getElementById('mem-soul-body').textContent.includes('Answer in German.')`, &back),
		tapsOpen,
		chromedp.Click(add+` > summary`, chromedp.ByQuery),
		tapsOpen, // a step just opened
		chromedp.WaitVisible(add+`[open] textarea`, chromedp.ByQuery),
		shot("soul-add-again"),
		chromedp.Evaluate(`document.querySelector(`+q(add+`[open] textarea`)+`).value === ''`, &empty),
		stayed(t, "Add", add, &at),
	)
	if err != nil || !back || !empty {
		t.Errorf("Add: %v; the line shows %v, the box is empty when it opens again %v", err, back, empty)
	}

	// A fact's page: a double tap on "No longer true". The button that
	// takes its place, "It's true again", is the same form.
	n = len(lm.calls())
	var again bool
	const valid = `form[action="/memory/facts/valid"]`
	err = run(ctx, visit(srv.URL+"/memory/facts/1"),
		markPlace(valid, valid, &at),
		doubleTap(valid+` button`),
		toastSays("ok", "Marked no longer true."),
		tapsOpen,
		shot("fact-invalidated"),
		stayed(t, "No longer true", valid, &at),
		chromedp.Evaluate(`document.querySelector('form[action="/memory/facts/valid"] input[name="valid"][value="true"]') !== null && document.getElementById('mem-fact-body').textContent.includes('No longer true since')`, &again),
	)
	if err != nil {
		t.Fatalf("No longer true: %v", err)
	}
	if got := lm.calls()[n:]; fmt.Sprint(got) != "[invalidate 1]" || !again {
		t.Errorf("a double tap on No longer true sent %v; the page offers It's true again %v", got, again)
	}

	// Then a double tap on Remove, and a finger's double tap near the right
	// edge of Restore, which takes Remove's place as the same form. The
	// second tap on Restore doesn't open the editor.
	n = len(lm.calls())
	removal := `form[action="/memory/facts/removal"] button`
	var edge []float64
	var loc string
	err = run(ctx, tapsOpen,
		markPlace(removal, removal, &at),
		doubleTap(removal),
		toastSays("ok", "Fact removed."),
		chromedp.Sleep(1200*time.Millisecond), // past the form's throttle
		chromedp.Evaluate(`(r => [r.right - 4, r.top + r.height / 2])(document.querySelector(`+q(removal)+`).getBoundingClientRect())`, &edge),
		finger(&edge),
		toastSays("ok", "Fact restored."),
		tapsOpen,
		chromedp.Location(&loc),
		stayed(t, "Remove, then Restore", removal, &at),
	)
	if got := lm.calls()[n:]; err != nil || fmt.Sprint(got) != "[delete 1 restore 1]" || loc != srv.URL+"/memory/facts/1" {
		t.Errorf("double taps on Remove, then Restore: %v; sent %v, want one of each; at %s", err, got, loc)
	}
}
