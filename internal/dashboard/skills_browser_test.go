package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// liveSkills is fakeSkillCtl whose Accept makes a proposed skill live, as the
// real control does. A browser test's server and the test share it.
type liveSkills struct {
	fakeSkillCtl
	mu sync.Mutex
}

func (f *liveSkills) Skills() []SkillView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.skills)
}

func (f *liveSkills) Accept(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acceptCalls = append(f.acceptCalls, name)
	i := slices.IndexFunc(f.skills, func(v SkillView) bool { return v.Name == name && v.Status == "proposed" })
	if i < 0 {
		return false, nil
	}
	f.skills[i].Status, f.skills[i].Enabled = "live", true
	return true, nil
}

// liveMCP is fakeMCPCtl whose trust changes stick.
type liveMCP struct{ fakeMCPCtl }

func (f *liveMCP) Servers() []MCPServerView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.servers)
}

func (f *liveMCP) SetTrust(target, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trustCalls = append(f.trustCalls, target+"="+mode)
	for i := range f.servers {
		if f.servers[i].Label == target {
			f.servers[i].Trust = mode
		}
	}
	return nil
}

// liveRules is fakeRules whose Remove takes the rule off the list.
type liveRules struct {
	fakeRules
	mu sync.Mutex
}

func (f *liveRules) Rules() []ToolRuleRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows)
}

func (f *liveRules) RemoveRule(p string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, p)
	i := slices.IndexFunc(f.rows, func(r ToolRuleRow) bool { return r.Pattern == p })
	if i < 0 {
		return false, nil
	}
	f.rows = slices.Delete(f.rows, i, i+1)
	return true, nil
}

// Skills, Remote services and Tools answer in place on a phone (DESIGN.md
// "Forms that don't reload"), signed in through the form: a change well
// down each page shows its toast, keeps the scroll and the page (a marker
// set before it survives), the row shows its new state, and the page fits.
// A double tap on Accept accepts one skill and on a rule's Remove removes
// one rule, though the second tap lands on the form now holding the next.
// TENANT_UI_SCREENSHOT=<dir> keeps screenshots.
func TestSkillsMCPToolsInlineInChrome(t *testing.T) {
	sk := &liveSkills{fakeSkillCtl: fakeSkillCtl{autoMode: "off"}}
	for i := 1; i <= 8; i++ {
		sk.skills = append(sk.skills, SkillView{Name: fmt.Sprintf("proposed-%02d", i), Description: "A way of working your agent figured out.", Status: "proposed"})
	}
	for i := 1; i <= 6; i++ {
		sk.skills = append(sk.skills, SkillView{Name: fmt.Sprintf("learned-%02d", i), Description: "A way of working you accepted.", Status: "live", Enabled: true})
	}
	mc := &liveMCP{}
	for i := 1; i <= 10; i++ {
		mc.servers = append(mc.servers, MCPServerView{Label: fmt.Sprintf("mcp:svc%02d", i), URL: fmt.Sprintf("https://svc%02d.example/mcp", i), Enabled: true, ToolCount: i, Trust: "ask"})
	}
	ru := &liveRules{}
	for i := 1; i <= 14; i++ {
		ru.rows = append(ru.rows, ToolRuleRow{Pattern: fmt.Sprintf("tool_%02d", i), Mode: "allow", Kind: "name"})
	}
	s := New(Config{Addr: "127.0.0.1:0", Auth: GenerateToken()}, nil, nil, nil, nil, nil)
	s.SetSkills(sk)
	s.SetMCP(mc)
	s.SetRules(ru)
	ctx, srv := chromeSignedIn(t, s, 390, 844, "/skills")

	q := strconv.Quote // a Go-quoted ASCII string is a JS string literal
	wait := chromedp.WithPollingTimeout(6 * time.Second)
	// inPlace taps sel's button in the middle of the screen, twice at one
	// spot 120ms apart when double, and checks the toast says toast, the
	// scroll and the page stayed, and the page fits.
	inPlace := func(page, sel string, double bool, toast string) {
		t.Helper()
		var xy, before, after []float64
		var kept string
		var bad []string
		const where = `[document.querySelector('.page').scrollTop, scrollY]`
		err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+page),
			chromedp.Evaluate(`(b => (b.scrollIntoView({block: 'center'}), window.inlineMark = 'kept', (r => [r.left + r.width / 2, r.top + r.height / 2])(b.getBoundingClientRect())))(document.querySelector(`+q(sel)+`))`, &xy),
			chromedp.Evaluate(where, &before),
			chromedp.ActionFunc(func(ctx context.Context) error {
				if err := chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx); err != nil || !double {
					return err
				}
				time.Sleep(120 * time.Millisecond)
				return chromedp.MouseClickXY(xy[0], xy[1]).Do(ctx)
			}),
			chromedp.Poll(`(t => t.classList.contains('ok') && t.textContent.includes(`+q(toast)+`))(document.getElementById('toast'))`, nil, wait),
			chromedp.Sleep(300*time.Millisecond), // room for a second post to land
			chromedp.ActionFunc(func(ctx context.Context) error {
				dir := os.Getenv("TENANT_UI_SCREENSHOT")
				if dir == "" {
					return nil
				}
				var png []byte
				if err := chromedp.CaptureScreenshot(&png).Do(ctx); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "inline-390"+strings.ReplaceAll(page, "/", "-")+".png"), png, 0o644)
			}),
			chromedp.Evaluate(where, &after),
			chromedp.Evaluate(`String(window.inlineMark)`, &kept),
			chromedp.Evaluate(fitsJS, &bad),
		)
		if err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		if len(before) != 2 || before[0] < 100 {
			t.Errorf("%s scrolled to %v before the tap, want well down it", page, before)
		}
		if fmt.Sprint(after) != fmt.Sprint(before) {
			t.Errorf("%s: the scroll moved from %v to %v", page, before, after)
		}
		if kept != "kept" {
			t.Errorf("%s reloaded: the marker is %q", page, kept)
		}
		for _, b := range bad {
			t.Errorf("%s with the toast: %s", page, b)
		}
	}
	count := func(sel string) (n int) {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll(`+q(sel)+`).length`, &n)); err != nil {
			t.Fatal(err)
		}
		return n
	}

	inPlace("/skills", `form[action="/skills/accept"]:has(input[value="proposed-05"]) button`, true, "Skill accepted: it's live now.")
	sk.mu.Lock()
	accepted := slices.Clone(sk.acceptCalls)
	sk.mu.Unlock()
	var learned string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('.card:has(form[action="/skills/enable"]) .card-head').textContent`, &learned)); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(accepted) != "[proposed-05]" || count(`form[action="/skills/accept"]`) != 7 || learned != "What my agent has learned (7)" {
		t.Errorf("a double tap on Accept sent %v and left %d to accept and %q, want one post, 7 and 7 learned", accepted, count(`form[action="/skills/accept"]`), learned)
	}

	inPlace("/mcp", `form[action="/mcp/trust"]:has(input[value="mcp:svc10"]):has(input[value="deny"]) button`, false, "mcp:svc10: your agent now can't use its tools.")
	var row string
	if err := chromedp.Run(ctx, chromedp.Text(`.soul-row:has(input[value="mcp:svc10"])`, &row, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	mc.mu.Lock()
	trusted := slices.Clone(mc.trustCalls)
	mc.mu.Unlock()
	if fmt.Sprint(trusted) != "[mcp:svc10=deny]" || !strings.Contains(row, "blocked") || !strings.Contains(row, "Ask first") || strings.Contains(row, "Block") {
		t.Errorf("Block sent %v and the row says %q, want one post and blocked with Ask first", trusted, row)
	}

	inPlace("/tools", `form[action="/tools/rules/remove"]:has(input[value="tool_08"]) button`, true, "Rule removed: tool_08.")
	ru.mu.Lock()
	removed := slices.Clone(ru.removes)
	ru.mu.Unlock()
	if left := count(`form[action="/tools/rules/remove"]`); fmt.Sprint(removed) != "[tool_08]" || left != 13 || count(`input[value="tool_08"]`) != 0 {
		t.Errorf("a double tap on a rule's Remove sent %v and left %d rules, want one post and 13", removed, left)
	}
}
