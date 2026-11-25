package dashboard

import (
	"fmt"
	"testing"

	"github.com/chromedp/chromedp"
)

// railScrollJS opens the rail, appends 200 lines the way the events stream
// does, and returns what is wrong: the rail has to stay inside the chat row
// and scroll there, the page must not get longer, and the newest line has to
// show, also after more lines arrive.
const railScrollJS = `(() => {
	const d = document.querySelector('.activity'), lines = document.querySelector('#activity-rail'),
		rail = lines.parentElement, page = document.querySelector('.page'), row = document.querySelector('.chat-row');
	const add = n => { for (let i = 0; i < n; i++) { const l = document.createElement('div'); l.className = 'line k-tool'; l.textContent = 'os_exec({"command": "& C:\\\\Tenant\\\\python...)'; lines.append(l) } };
	const newest = () => Math.round(lines.lastElementChild.getBoundingClientRect().bottom - rail.getBoundingClientRect().bottom);
	const bad = [];
	d.open = true;
	add(200);
	if (page.scrollHeight > page.clientHeight) bad.push('the page scrolls: ' + page.scrollHeight + ' in ' + page.clientHeight);
	if (rail.scrollHeight <= rail.clientHeight) bad.push('the rail does not scroll: ' + rail.scrollHeight + ' in ' + rail.clientHeight);
	if (d.getBoundingClientRect().bottom > row.getBoundingClientRect().bottom + 1) bad.push('the rail leaves the chat row');
	if (newest() > 0) bad.push('the newest line is ' + newest() + 'px under the rail');
	add(20);
	if (newest() > 0) bad.push('after more lines the newest is ' + newest() + 'px under the rail');
	return bad.join('; ');
})()`

// The activity rail scrolls on its own beside the chat, and under it on a
// phone (DESIGN.md "The activity rail"): a long run's lines never make the
// page longer or run under the message box, and the rail follows the newest.
func TestChatRailScrollsInChrome(t *testing.T) {
	s := harnessServer(t, Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, populated, nil)
	for _, size := range [][2]int{{1280, 800}, {390, 844}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			ctx, _ := chromeSignedIn(t, s, size[0], size[1], "/chat")
			var bad string
			if err := run(ctx, chromedp.WaitReady(`.activity summary`, chromedp.ByQuery), chromedp.Evaluate(railScrollJS, &bad)); err != nil {
				t.Fatal(err)
			}
			if bad != "" {
				t.Error(bad)
			}
		})
	}
}
