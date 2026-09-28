package dashboard

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeGoal is a goal loop: Clear stops a running one.
type fakeGoal struct {
	mu     sync.Mutex
	view   GoalStatusView
	clears int
}

func (f *fakeGoal) Show() GoalStatusView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view
}

func (f *fakeGoal) Clear() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears++
	was := f.view.Active
	f.view = GoalStatusView{}
	return was
}

func (f *fakeGoal) clearCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clears
}

func activeGoal() GoalStatusView {
	return GoalStatusView{Active: true, Condition: "keep the build green", Turns: 3, MaxTurns: 20, Elapsed: "12m30s", LastCheck: "the tests still fail"}
}

// The harness: populated is a running goal at its widest (an unbroken
// condition, a latest check with a long URL); failing is the idle page, since
// the goal control has no read that can fail.
func init() {
	harnessFakes = append(harnessFakes, func(s *Server, mode fakeMode) {
		f := &fakeGoal{}
		if mode == populated {
			f.view = GoalStatusView{Active: true, Condition: longName(1) + " " + longURL(1), Turns: 12, MaxTurns: 20, Elapsed: "1h2m3s", LastCheck: longName(2) + " " + longURL(2)}
		}
		s.SetGoal(f)
	})
}

func goalServer(g GoalControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if g != nil {
		s.SetGoal(g)
	}
	return s
}

// The page's states (DESIGN.md §6): off under serve, empty when idle, busy
// while a goal runs (with its Stop, and no confirm step), the brief "reached"
// view, and both flash kinds.
func TestGoal_States(t *testing.T) {
	stop := `<form method="POST" action="/goal/stop"`
	for _, c := range []struct {
		name, query string
		goal        GoalControl
		want, not   []string
	}{
		{"off", "", nil,
			[]string{`class="card state-off"`, "Autopilot isn't available here. It runs inside the terminal app"},
			[]string{stop}},
		{"empty", "", &fakeGoal{},
			[]string{`<div class="empty">Autopilot is off.`},
			[]string{stop, "state-off", `class="busy"`}},
		{"busy", "", &fakeGoal{view: activeGoal()},
			[]string{`<div class="busy" role="status">`, "Your agent is working on its own", "12m30s", stop, "Stop after this step",
				"keep the build green", "Steps finished: 3 of at most 20 · running for 12m30s", "Latest check:", "the tests still fail"},
			[]string{`name="confirm"`, `class="empty"`, "Goal reached"}},
		{"met", "", &fakeGoal{view: GoalStatusView{Active: true, Met: true, Condition: "keep the build green"}},
			[]string{"Goal reached", "keep the build green"},
			[]string{stop, `class="busy"`}},
		{"error", "?err=" + url.QueryEscape("Autopilot isn't running."), &fakeGoal{},
			[]string{`<div class="banner err" role="alert">Autopilot isn't running.</div>`}, nil},
		{"success", "?msg=Stopped.", &fakeGoal{},
			[]string{`<div class="banner ok" role="status">Stopped.</div>`}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := get(t, goalServer(c.goal), "/goal"+c.query)
			body := html.UnescapeString(rec.Body.String())
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /goal = %d", rec.Code)
			}
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(body, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
}

// Stop needs no confirm step, calls Clear once, and says what happened; with
// nothing running it says so, and with no control it only goes back.
func TestGoal_Stop(t *testing.T) {
	for _, c := range []struct {
		name  string
		goal  *fakeGoal
		flash string
	}{
		{"running", &fakeGoal{view: activeGoal()}, "msg=" + url.QueryEscape("Stopped. The step in progress finishes, then no new step starts.")},
		{"idle", &fakeGoal{}, "err=" + url.QueryEscape("Autopilot isn't running.")},
		{"off", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var g GoalControl
			if c.goal != nil {
				g = c.goal
			}
			rec := submitForm(t, goalServer(g), "/goal/stop", "")
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("POST /goal/stop = %d, want 303", rec.Code)
			}
			want := "/goal"
			if c.flash != "" {
				want += "?" + c.flash
			}
			if loc := rec.Header().Get("Location"); loc != want {
				t.Errorf("Location = %q, want %q", loc, want)
			}
			if c.goal != nil && c.goal.clearCalls() != 1 {
				t.Errorf("Clear called %d times, want 1", c.goal.clearCalls())
			}
		})
	}
}

// The condition and the latest check are escaped, and their hidden
// characters show (F10): the check is the model's own words.
func TestGoal_EscapesAndShowsHidden(t *testing.T) {
	body := get(t, goalServer(&fakeGoal{view: GoalStatusView{Active: true, MaxTurns: 20,
		Condition: "<script>alert(1)</script>", LastCheck: "fine‮<b>x</b>"}}), "/goal").Body.String()
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<b>x") {
		t.Error("the condition or the check reached the page unescaped")
	}
	for _, w := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "fine⟨U&#43;202E⟩&lt;b&gt;x&lt;/b&gt;"} {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in:\n%s", w, body[strings.Index(body, `<section class="page"`):])
		}
	}
}

// Stop rides the dashboard's CSRF check like every form: a signed-in POST
// with no Origin is refused before Clear, and the same-origin one works.
func TestGoal_StopRefusesCSRF(t *testing.T) {
	g := &fakeGoal{view: activeGoal()}
	s := New(Config{Addr: "127.0.0.1:8770", Auth: "secret"}, nil, nil, nil, nil, nil)
	s.SetGoal(g)
	post := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, "/goal/stop", strings.NewReader(""))
		req.Host = "127.0.0.1:8770"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: dashCookieName, Value: "secret"})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		s.served().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(""); code != http.StatusForbidden || g.clearCalls() != 0 {
		t.Errorf("no Origin: %d with %d Clear calls, want 403 and none", code, g.clearCalls())
	}
	if code := post("http://127.0.0.1:8770"); code != http.StatusSeeOther || g.clearCalls() != 1 {
		t.Errorf("same origin: %d with %d Clear calls, want 303 and one", code, g.clearCalls())
	}
}
