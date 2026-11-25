package dashboard

import (
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The chat follows its newest line (theme.js): lines that arrive keep the log
// at the bottom; scrolled up it stays where you put it; sending a message
// jumps back down.
func TestChatFollowsInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, wireFakeRunner{})
	ctx, _ := chromeSignedIn(t, s, 1280, 700, "/chat")
	const (
		log      = `document.getElementById('chat-log')`
		add      = `(n => { for (let i = 0; i < n; i++) { const d = document.createElement('div'); d.className = 'msg'; d.textContent = 'line ' + i; ` + log + `.append(d) } })`
		atBottom = `(el => el.scrollHeight > el.clientHeight && el.scrollHeight - el.scrollTop - el.clientHeight < 2)(` + log + `)`
	)
	wait := chromedp.WithPollingTimeout(5 * time.Second)
	var top float64
	err := run(ctx,
		chromedp.WaitReady(`#chat-log`, chromedp.ByQuery),
		chromedp.Evaluate(add+`(80)`, nil),
		chromedp.Poll(atBottom, nil, wait), // arriving lines keep it at the bottom
		chromedp.Evaluate(log+`.scrollTop = 0`, nil),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(add+`(5)`, nil),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(log+`.scrollTop`, &top),
		chromedp.SendKeys(`#chat-input`, "hello\r", chromedp.ByQuery),
		chromedp.Poll(atBottom, nil, wait), // sending jumps back down
	)
	if err != nil {
		t.Fatalf("chat follow: %v", err)
	}
	if top != 0 {
		t.Errorf("scrolled up, a new line moved the log to %v; want it left at 0", top)
	}
}
