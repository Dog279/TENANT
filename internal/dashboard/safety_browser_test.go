package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"tenant/internal/agent"
)

// The live approval queue on a phone (390x844), signed in through the form
// (TEN-207): a request appears within a second of its event, fits the screen
// however long its detail, keeps an open step open across a re-render, is
// answered once however fast the taps, shows above the Chat composer, and
// the page says when live updates stop. TENANT_UI_SCREENSHOT=<dir> keeps
// screenshots.
func TestSafetyQueueInChrome(t *testing.T) {
	c := installCapture(t)
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, agent.NewBroker(0), nil)
	fq := &fakeQueue{}
	s.SetApprovals(fq)
	s.SetSafety(&fakeSafety{perms: safetyPerms()}, func() SafetyFacts { return SafetyFacts{Account: "dylan", Sandboxed: true} })
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/safety")
	shot := func(name string) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			dir := os.Getenv("TENANT_UI_SCREENSHOT")
			if dir == "" {
				return nil
			}
			var png []byte
			if err := chromedp.FullScreenshot(&png, 80).Do(ctx); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "safety-"+name+".jpg"), png, 0o644)
		})
	}
	// awaitStream waits for the n-th /events stream to have opened: it
	// subscribes right after this record, then sends its snapshot.
	awaitStream := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			got := 0
			for _, l := range c.lines() {
				if strings.HasPrefix(l, "1001 ") && strings.Contains(l, "stream=events") {
					got++
				}
			}
			if got >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the /events stream (#%d) never opened: %q", n, c.lines())
			}
		}
	}
	publish := func(kind agent.EventKind) chromedp.Action {
		return chromedp.ActionFunc(func(context.Context) error { s.broker.Publish(agent.Event{Kind: kind}); return nil })
	}
	awaitStream(1)

	// A published request shows within a second, and fits the phone.
	fq.add(PendingApproval{ID: "ap-1", Category: "exec", Action: "os_exec", Detail: longDetail(), AgeSecs: 5, Origin: "local", SessionOK: true})
	start := time.Now()
	var bad []string
	err := chromedp.Run(ctx,
		publish(agent.EventApprovalPending),
		chromedp.Poll(`document.querySelector('#q-ap-1') !== null`, nil, chromedp.WithPollingTimeout(time.Second), chromedp.WithPollingInterval(20*time.Millisecond)),
		chromedp.Evaluate(fitsJS, &bad),
		shot("queue"),
	)
	if err != nil {
		t.Fatalf("the request didn't show within a second: %v", err)
	}
	t.Logf("shown %v after its event", time.Since(start).Round(time.Millisecond))
	for _, b := range bad {
		t.Errorf("/safety with a long request at 390px: %s", b)
	}

	// An open session step stays open while the queue is re-rendered around
	// it (a second request arriving).
	var open bool
	fq.add(PendingApproval{ID: "ap-2", Category: "web", Action: "web_click", Detail: "click Buy", AgeSecs: 1, Origin: "local", SessionOK: true})
	err = chromedp.Run(ctx,
		chromedp.Click(`#q-ap-1 .q-session > summary`, chromedp.ByQuery),
		chromedp.WaitVisible(`#q-ap-1 .q-session[open] .confirm-box`, chromedp.ByQuery),
		shot("session-step"),
		publish(agent.EventApprovalPending),
		chromedp.WaitVisible(`#q-ap-2`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('#q-ap-1 .q-session').open`, &open),
		chromedp.Click(`#q-ap-1 .q-session > summary`, chromedp.ByQuery), // Cancel
	)
	if err != nil {
		t.Fatalf("the session step: %v", err)
	}
	if !open {
		t.Error("the open session step closed when the queue re-rendered")
	}

	// A double tap on Allow once answers once; the item goes and the note says so.
	var note string
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`(() => { const b = document.querySelector('#q-ap-1 .q-actions .btn.on'); b.click(); b.click(); return true })()`, nil),
		chromedp.WaitNotPresent(`#q-ap-1`, chromedp.ByQuery),
		chromedp.Text(`#approval-note`, &note, chromedp.ByQuery),
		publish(agent.EventApprovalResolved),
		chromedp.Sleep(300*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("Allow once: %v", err)
	}
	if got := fq.calls(); !slices.Equal(got, []string{"ap-1|approve"}) {
		t.Errorf("Decide calls %q, want one approve", got)
	}
	// The handler skips Decide for an item already gone, so count what
	// reached the server: one post, the second tap met a disabled button.
	posts := 0
	for _, l := range c.lines() {
		if strings.HasPrefix(l, "9001 start POST /safety/approvals/{id} ") {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("%d decision posts reached the server, want 1", posts)
	}
	if !strings.Contains(note, "Allowed once. Your agent is carrying on.") {
		t.Errorf("the note says %q", note)
	}

	// Chat shows the same queue above its composer.
	var above bool
	err = chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/chat"),
		chromedp.WaitVisible(`.chat-q #q-ap-2`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('.chat-q').compareDocumentPosition(document.querySelector('.composer')) === Node.DOCUMENT_POSITION_FOLLOWING`, &above),
		chromedp.Evaluate(fitsJS, &bad),
		shot("chat"),
	)
	if err != nil {
		t.Fatalf("the queue on Chat: %v", err)
	}
	if !above {
		t.Error("the queue isn't above the composer")
	}
	for _, b := range bad {
		t.Errorf("/chat with a request at 390px: %s", b)
	}

	// The stream drops: the page says live updates paused, and the stream's
	// snapshot on reconnect clears it.
	const paused = `getComputedStyle(document.querySelector('.live-off')).display !== 'none'`
	err = chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/safety"),
		chromedp.WaitVisible(`#q-ap-2`, chromedp.ByQuery),
		chromedp.ActionFunc(func(context.Context) error {
			awaitStream(3)
			srv.CloseClientConnections()
			return nil
		}),
		chromedp.Poll(paused, nil, chromedp.WithPollingTimeout(3*time.Second)),
		chromedp.Poll(`!(`+paused+`)`, nil, chromedp.WithPollingTimeout(10*time.Second)),
	)
	if err != nil {
		t.Fatalf("the paused banner across a reconnect: %v", err)
	}

	// The server stops: the banner stays.
	err = chromedp.Run(ctx,
		chromedp.ActionFunc(func(context.Context) error {
			awaitStream(4)
			srv.Listener.Close()
			srv.CloseClientConnections()
			return nil
		}),
		chromedp.Poll(paused, nil, chromedp.WithPollingTimeout(3*time.Second)),
	)
	if err != nil {
		t.Fatalf("the paused banner after the server stopped: %v", err)
	}
}
