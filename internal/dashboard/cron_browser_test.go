package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// inlineChrome is what the Scheduled jobs and Quality browser tests share: a
// phone signed in to page, and the checks for an answer in place.
type inlineChrome struct {
	t    *testing.T
	ctx  context.Context
	name string
}

func newInlineChrome(t *testing.T, s *Server, page, name string) inlineChrome {
	ctx, _ := chromeSignedIn(t, s, 390, 844, page)
	c := inlineChrome{t, ctx, name}
	// Datastar is up once the status strip's first GET has come back.
	c.run("waiting for Datastar", chromedp.Poll(`performance.getEntriesByType('resource').some(e => e.name.includes('/ui/status'))`, nil, chromedp.WithPollingTimeout(6*time.Second)))
	return c
}

func (c inlineChrome) run(step string, actions ...chromedp.Action) {
	c.t.Helper()
	if err := chromedp.Run(c.ctx, actions...); err != nil {
		c.t.Fatalf("%s: %v", step, err)
	}
}

// toast waits for the toast of kind ok or err to say text.
func (inlineChrome) toast(kind, text string) chromedp.Action {
	q := strconv.Quote // a Go-quoted ASCII string is a JS string literal
	return chromedp.Poll(`(t => t.classList.contains(`+q(kind)+`) && t.textContent.includes(`+q(text)+`))(document.getElementById('toast'))`, nil, chromedp.WithPollingTimeout(6*time.Second))
}

// shot keeps a screenshot when TENANT_UI_SCREENSHOT=<dir>.
func (c inlineChrome) shot(name string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		dir := os.Getenv("TENANT_UI_SCREENSHOT")
		if dir == "" {
			return nil
		}
		var png []byte
		if err := chromedp.CaptureScreenshot(&png).Do(ctx); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, c.name+"-390-"+name+".png"), png, 0o644)
	})
}

// settle is longer than the half second after a post in which the
// Scheduled jobs page drops another.
const settle = 600 * time.Millisecond

// doubleTap taps the middle of sel twice at one spot, 120ms apart.
func doubleTap(sel string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var xy []float64
		if err := chromedp.Evaluate(`(r => [r.left + r.width / 2, r.top + r.height / 2])(document.querySelector(`+strconv.Quote(sel)+`).getBoundingClientRect())`, &xy).Do(ctx); err != nil {
			return err
		}
		if err := chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx); err != nil {
			return err
		}
		time.Sleep(120 * time.Millisecond)
		return chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx)
	})
}

// inPlace waits out settle, scrolls sel to the middle of the screen, marks
// the page, runs act and then checks the scroll and the page are what they
// were, and that the page fits.
func (c inlineChrome) inPlace(step, sel string, act ...chromedp.Action) {
	c.t.Helper()
	const where = `[document.querySelector('.page').scrollTop, scrollY]`
	var before, after []float64
	var kept string
	var bad []string
	c.run(step, chromedp.Sleep(settle), chromedp.Evaluate(`document.querySelector(`+strconv.Quote(sel)+`).scrollIntoView({block: 'center'}); window.inlineMark = 'kept'; `+where, &before))
	c.run(step, act...)
	c.run(step, chromedp.Evaluate(where, &after), chromedp.Evaluate(`String(window.inlineMark)`, &kept), chromedp.Evaluate(fitsJS, &bad))
	if len(before) != 2 || before[0] < 100 {
		c.t.Fatalf("%s: the page scrolled to %v before, want well down it", step, before)
	}
	if fmt.Sprint(after) != fmt.Sprint(before) {
		c.t.Errorf("%s: the scroll moved from %v to %v", step, before, after)
	}
	if kept != "kept" {
		c.t.Errorf("%s: the page reloaded: the marker is %q", step, kept)
	}
	for _, b := range bad {
		c.t.Errorf("%s: %s", step, b)
	}
}

// Scheduled jobs answer in place on a phone (DESIGN.md "Forms that don't
// reload"): a double tap on the exec switch's Turn off turns it off once
// and doesn't open the Turn on step that moves under it; turning off a job
// far down the page keeps the scroll and the page, and its card says off; a
// double tap on Run now runs it once; a
// double tap on Delete's Yes removes the card, closes the step and runs
// nothing; a job added is listed and the add form comes back empty; a
// failed add keeps what was typed.
func TestCronInlineFormsInChrome(t *testing.T) {
	var jobs []CronJobView
	for i := 1; i <= 6; i++ {
		jobs = append(jobs, CronJobView{ID: fmt.Sprintf("j%d", i), Name: fmt.Sprintf("job %d", i), Spec: "0 9 * * 1-5",
			Prompt: "Read the day's messages and post a short summary to Discord.", Enabled: true, NextRun: "2026-09-29 09:00"})
	}
	fc := &fakeCronCtl{jobs: jobs, execOn: true}
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, nil, nil)
	s.SetCron(fc)
	c := newInlineChrome(t, s, "/cron", "cron")

	// After Turn off, the Turn on step's button is where Turn off was: the
	// page doesn't let the second tap open it.
	var stepOpen bool
	c.run("a double tap on the exec switch's Turn off",
		doubleTap(`form[action="/cron/exec"] button`),
		c.toast("ok", "Scheduled jobs can't make changes now."),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`document.querySelector('.page details[open]') !== null`, &stepOpen),
		c.shot("exec-off"),
	)
	fc.set(func() {
		if stepOpen || fmt.Sprint(fc.execCalls) != "[false]" {
			t.Errorf("a double tap on Turn off: a confirm step open %v, exec calls %v; want none open and one call to turn it off", stepOpen, fc.execCalls)
		}
	})

	var head, btn string
	c.inPlace("Turn off", "#cron-j6",
		chromedp.Click(`#cron-j6 form[action="/cron/enable"] button`, chromedp.ByQuery),
		c.toast("ok", "job 6 is off."),
		c.shot("turn-off"),
		chromedp.Text(`#cron-j6 .ghead`, &head, chromedp.ByQuery),
		chromedp.Text(`#cron-j6 form[action="/cron/enable"] button`, &btn, chromedp.ByQuery),
	)
	if !strings.Contains(head, "off") || btn != "Turn on" {
		t.Errorf("Turn off: the card says %q with a %q button, want off and Turn on", head, btn)
	}

	// Two taps at one spot, 120ms apart: the morph keeps the card's form,
	// whose throttle drops the second.
	c.inPlace("a double tap on Run now", "#cron-j6",
		doubleTap(`#cron-j6 form[action="/cron/run"] button`),
		c.toast("ok", "job 6 is running now."),
		chromedp.Sleep(300*time.Millisecond), // room for a second post to land
	)
	fc.set(func() {
		if fmt.Sprint(fc.runCalls) != "[j6]" {
			t.Errorf("a double tap on Run now ran %v, want j6 once", fc.runCalls)
		}
	})

	// On a phone Delete's Yes sits at the foot of the screen, and once j4's
	// card goes, j6's Run now is under it: the page, not the form, drops the
	// second tap. (Not scroll-checked: a card near the end shortens the page.)
	var gone bool
	var kept string
	c.run("a double tap on Delete's Yes",
		chromedp.Sleep(settle),
		chromedp.Evaluate(`document.getElementById('cron-j4').scrollIntoView({block: 'center'})`, nil),
		chromedp.Click(`#cron-j4 details.confirm-step > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(`#cron-j4 details[open] .confirm-box button`, chromedp.ByQuery),
		doubleTap(`#cron-j4 details[open] .confirm-box button`),
		c.toast("ok", "job 4 is deleted."),
		c.shot("delete"),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`document.getElementById('cron-j4') === null && document.querySelector('.page details[open]') === null`, &gone),
		chromedp.Evaluate(`String(window.inlineMark)`, &kept),
		chromedp.Click(`#toast .toast-x`, chromedp.ByQuery),
	)
	fc.set(func() {
		if !gone || kept != "kept" || fmt.Sprint(fc.removeCalls) != "[j4]" || fmt.Sprint(fc.runCalls) != "[j6]" {
			t.Errorf("a double tap on Delete's Yes: card gone and step closed %v, marker %q, removed %v, ran %v; want j4 removed and nothing run", gone, kept, fc.removeCalls, fc.runCalls)
		}
	})

	// The add form is at the top: typing there scrolls the page to it.
	const typed = `[...document.querySelectorAll('#jobs-add input[type=text]')].map(i => i.value).join('|')`
	var left string
	var listed bool
	c.run("Add",
		chromedp.Sleep(settle),
		chromedp.SendKeys(`#jobs-add input[name=spec]`, "@weekly", chromedp.ByQuery),
		chromedp.SendKeys(`#jobs-add input[name=prompt]`, "Send the weekly report.", chromedp.ByQuery),
		chromedp.Click(`#jobs-add button[type=submit]`, chromedp.ByQuery),
		c.toast("ok", "Job scheduled."),
		c.shot("add"),
		chromedp.Evaluate(typed, &left),
		chromedp.Evaluate(`document.getElementById('cron-new1') !== null`, &listed),
		chromedp.Evaluate(`String(window.inlineMark)`, &kept),
	)
	if left != "|||" || !listed || kept != "kept" {
		t.Errorf("Add: the boxes hold %q (want empty), the job listed %v, marker %q", left, listed, kept)
	}

	fc.set(func() { fc.addErr = errors.New("bad schedule") })
	c.run("a failed add",
		chromedp.Sleep(settle),
		chromedp.SendKeys(`#jobs-add input[name=spec]`, "every tuesday", chromedp.ByQuery),
		chromedp.SendKeys(`#jobs-add input[name=prompt]`, "Report.", chromedp.ByQuery),
		chromedp.Click(`#jobs-add button[type=submit]`, chromedp.ByQuery),
		c.toast("err", "Couldn't schedule that job."),
		c.shot("add-error"),
		chromedp.Evaluate(typed, &left),
	)
	if left != "|every tuesday||Report." {
		t.Errorf("a failed add: the boxes hold %q, want what was typed", left)
	}
}

// Quality answers in place on a phone: saving the schedule far down the
// page keeps the scroll and the page and shows the new schedule; setting
// the grader keeps its section open; Run now shows the check in progress.
func TestEvalInlineFormsInChrome(t *testing.T) {
	var points []EvalTrendPoint
	for i := range 8 {
		points = append(points, EvalTrendPoint{When: fmt.Sprintf("09-%02d 03:15", 20+i), Score: float64(70 + i), HasBase: true})
	}
	fc := &fakeEvalCtl{sched: EvalScheduleView{Desc: "every 24h0m0s", Live: true, HasRun: true, LastWhen: "2026-09-27 03:15", LastScore: 81, LastPass: 40, LastTotal: 50, Trend: "up"},
		points: points, diff: "recall-dates   0.60 -> 0.90\nsummarize-log  0.80 -> 0.70", judge: "Your main model grades the checks."}
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, nil, nil)
	s.SetEval(fc)
	c := newInlineChrome(t, s, "/eval", "eval")

	var now string
	var at bool
	c.run("choosing Daily at",
		chromedp.Click(`input[name=mode][value=at]`, chromedp.ByQuery),
		chromedp.SendKeys(`input[name=spec_at]`, "04:30", chromedp.ByQuery),
	)
	c.inPlace("Save schedule", `form[action="/eval/schedule"] button`,
		chromedp.Click(`form[action="/eval/schedule"] button`, chromedp.ByQuery),
		c.toast("ok", "Schedule saved."),
		c.shot("schedule"),
		chromedp.Evaluate(`document.querySelector('.card:has(> form[action="/eval/schedule"]) .mb').textContent`, &now),
		chromedp.Evaluate(`document.querySelector('input[name=mode][value=at]').checked`, &at),
	)
	if !strings.Contains(now, "daily at 04:30") || !at {
		t.Errorf("Save schedule: the card says %q, Daily at chosen %v", now, at)
	}

	var open bool
	var judge string
	c.run("typing a grader",
		chromedp.Click(`details.advanced > summary`, chromedp.ByQuery),
		chromedp.SendKeys(`form[action="/eval/judge"] input[name=kind]`, "zai", chromedp.ByQuery),
		chromedp.SendKeys(`form[action="/eval/judge"] input[name=model]`, "glm-4.6", chromedp.ByQuery),
	)
	c.inPlace("Set the grader", `form[action="/eval/judge"] button`,
		chromedp.Click(`form[action="/eval/judge"] button`, chromedp.ByQuery),
		c.toast("ok", "Saved: glm-4.6 from zai grades the checks now."),
		chromedp.Evaluate(`document.querySelector('details.advanced').open`, &open),
		chromedp.Text(`details.advanced > .dim`, &judge, chromedp.ByQuery),
	)
	if !open || judge != "glm-4.6 from zai grades the checks." {
		t.Errorf("Set the grader: the section open %v, it says %q", open, judge)
	}

	// Last: the check in progress reloads the page every 20s.
	var loading string
	var kept string
	c.run("Run now",
		chromedp.Click(`form[action="/eval/run"] button`, chromedp.ByQuery),
		c.toast("ok", "Quality check queued."),
		chromedp.Evaluate(`document.querySelector('.loading').textContent`, &loading),
		c.shot("run"),
		chromedp.Evaluate(`String(window.inlineMark)`, &kept),
	)
	if loading != "Check in progress" || kept != "kept" {
		t.Errorf("Run now: the page shows %q (want the check in progress), marker %q", loading, kept)
	}
}
