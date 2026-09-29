package dashboard

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Every form on Approvals & safety, Helpers, Research and Autopilot answers a
// Datastar post in place (DESIGN.md "Forms that don't reload"): a 200 stream
// with #toast and the page's region, never a 303 the fetch would follow. A
// create swaps its region in whole, so its form starts empty. A refusal is
// an alert and leaves the region out, so an open step keeps what was typed.
// The same post without Datastar still gets the 303 and its flash.
func TestV2Pages_InlineAnswers(t *testing.T) {
	safety := func() *Server { return safetyServer(&fakeSafety{perms: safetyPerms()}, &fakeQueue{}, SafetyFacts{}) }
	agents := func() *Server { return agentsServer(sampleAgents(), cloudModels...) }
	research := func() *Server {
		f := newFakeResearch()
		f.rows, f.stopOK = []ResearchRunRow{{ID: "r-1", Question: "What is fakeness?", StateText: "Finished"}}, true
		f.live = ResearchLiveView{Phase: "writing", Running: true}
		return researchServer(f)
	}
	goal := func() *Server { return goalServer(&fakeGoal{view: activeGoal()}) }
	const (
		modes   = `<div id="safety-modes">`
		helpers = `<div id="agents-body">`
		ask     = `<div id="research-ask">`
		goalRgn = `<div id="goal-body">`
	)
	for _, c := range []struct {
		name, path string
		form       url.Values
		srv        func() *Server
		toast      string // "" = an empty toast
		err        bool   // an alert, and no region
		region     string // the region's opening tag
		has, lacks string // in the answer, or not
		create     bool
	}{
		{name: "safety deny", path: "/safety/mode", form: url.Values{"category": {"web"}, "mode": {"deny"}}, srv: safety, toast: "Act on websites: your agent can't do this now.", region: modes,
			has: `<button type="button" class="btn sm on" disabled aria-pressed="true">Deny</button>`},
		{name: "safety allow", path: "/safety/mode", form: url.Values{"category": {"web"}, "mode": {"allow"}, "confirm": {"yes"}}, srv: safety, toast: "Act on websites: your agent goes ahead without asking.", region: modes},
		{name: "safety ask me again", path: "/safety/mode", form: url.Values{"category": {"exec"}, "mode": {"ask"}}, srv: safety, toast: "Run commands: your agent asks you again from now on.", region: modes, lacks: "Ask me again"},
		{name: "safety keep new", path: "/safety/new", srv: safety, toast: "Kept at Ask: Share private data, Connect remote services, Leave the sandbox. Your agent asks you first.", region: modes, lacks: "/safety/new"},
		{name: "safety refused", path: "/safety/mode", form: url.Values{"category": {"nope"}, "mode": {"ask"}}, srv: safety, toast: "That safety check doesn't exist. Nothing changed.", err: true, region: modes},
		{name: "safety off", path: "/safety/new", srv: func() *Server { return safetyServer(nil, &fakeQueue{}, SafetyFacts{}) }, toast: modesOff, err: true, region: modes},

		{name: "helper add", path: "/agents/add", form: url.Values{"name": {"h1"}, "provider": {"cloud"}}, srv: agents, toast: "Added h1. Your main agent learns", region: helpers, create: true},
		{name: "helper model", path: "/agents/model", form: url.Values{"name": {"QA"}, "provider": {"cloud"}}, srv: agents, toast: "QA will use cloud from its next task.", region: helpers, has: `id="edit"`},
		{name: "helper instructions", path: "/agents/soul", form: url.Values{"name": {"QA"}, "soul": {"Be thorough."}}, srv: agents, toast: "Saved QA's instructions.", region: helpers, has: `id="edit"`},
		{name: "helper clear", path: "/agents/soul", form: url.Values{"name": {"QA"}, "confirm": {"yes"}}, srv: agents, toast: "Cleared QA's instructions.", region: helpers, has: `id="edit"`},
		{name: "helper rename", path: "/agents/rename", form: url.Values{"name": {"researcher"}, "new_name": {"reader"}}, srv: agents, toast: "Renamed researcher to reader.", region: helpers, lacks: `id="edit"`},
		{name: "helper remove", path: "/agents/remove", form: url.Values{"name": {"researcher"}, "confirm": {"yes"}}, srv: agents, toast: "Removed researcher.", region: helpers},
		{name: "helper reset", path: "/agents/reset", form: url.Values{"name": {"QA"}, "confirm": {"yes"}}, srv: agents, toast: "QA is back to its built-in model and instructions.", region: helpers},
		{name: "helper refused", path: "/agents/add", form: url.Values{"name": {"h1"}}, srv: agents, toast: "Pick a model provider for the helper.", err: true, region: helpers},
		{name: "helpers off", path: "/agents/remove", form: url.Values{"name": {"x"}, "confirm": {"yes"}}, srv: func() *Server { return agentsServer(nil) }, region: helpers,
			has: "Helpers aren't available on this Tenant."},

		{name: "research start", path: "/research/start", form: url.Values{"question": {"Why?"}}, srv: research, toast: "Research started. Its progress shows below.", region: ask, create: true,
			has: `<div id="research-live">`},
		{name: "research clarify", path: "/research/clarify", form: url.Values{"answer": {"2024"}}, srv: research, toast: "Research started.", region: ask, has: `<div id="research-history">`},
		// The run's still winding down: Stop's step is there, and closes.
		{name: "research stop", path: "/research/stop", form: url.Values{"confirm": {"yes"}}, srv: research, toast: "Stopping the research.", region: ask,
			has: `<details class="confirm-step"><summary class="btn sm danger"><span class="c-open">Stop</span>`},
		{name: "research replay", path: "/research/replay", form: url.Values{"run": {"r-1"}}, srv: research, toast: "Running it again with the current model.", region: ask},
		{name: "research delete", path: "/research/delete", form: url.Values{"run": {"r-1"}, "confirm": {"yes"}}, srv: research, toast: "Deleted from the list.", region: ask, lacks: `value="r-1"`},
		{name: "research refused", path: "/research/start", form: url.Values{"question": {" "}}, srv: research, toast: "Type a question first.", err: true, region: ask},

		{name: "autopilot stop", path: "/goal/stop", srv: goal, toast: "Stopped. The step in progress finishes, then no new step starts.", region: goalRgn, has: "Autopilot is off.", lacks: `class="busy"`},
		{name: "autopilot refused", path: "/goal/stop", srv: func() *Server { return goalServer(&fakeGoal{}) }, toast: "Autopilot isn't running.", err: true, region: goalRgn},
		{name: "autopilot off", path: "/goal/stop", srv: func() *Server { return goalServer(nil) }, region: goalRgn, has: "Autopilot isn't available here."},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := sendForm(c.srv().Handler(), http.MethodPost, c.path, c.form, datastar)
			body := rec.Body.String()
			plain := html.UnescapeString(body)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Location") != "" {
				t.Fatalf("%d %q to %q, want a 200 text/event-stream:\n%s", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Location"), body)
			}
			toast := `<div id="toast"></div>`
			switch {
			case c.err:
				toast = `<div id="toast" class="toast err" role="alert"><span class="toast-text">` + c.toast
			case c.toast != "":
				toast = `<div id="toast" class="toast ok"><span class="toast-text">` + c.toast
			}
			if !strings.Contains(plain, "data: mode replace\ndata: elements "+toast) || !strings.Contains(body, `data: elements <div id="ui-status"`) {
				t.Errorf("want the toast %q and the status strip:\n%s", toast, body)
			}
			patch := "event: datastar-patch-elements\ndata: elements " + c.region
			if c.create {
				patch = "event: datastar-patch-elements\ndata: mode replace\ndata: elements " + c.region
			}
			if got := strings.Contains(body, patch); got == c.err {
				t.Errorf("region patch %q sent %v, want %v:\n%s", patch, got, !c.err, body)
			}
			if c.has != "" && !strings.Contains(plain, c.has) {
				t.Errorf("the answer lacks %q:\n%s", c.has, body)
			}
			if c.lacks != "" && strings.Contains(plain, c.lacks) {
				t.Errorf("the answer still has %q:\n%s", c.lacks, body)
			}

			rec = sendForm(c.srv().Handler(), http.MethodPost, c.path, c.form, nil)
			if page := strings.SplitN(c.path, "/", 3)[1]; rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/"+page) {
				t.Errorf("a plain post: %d to %q, want a 303 to /%s", rec.Code, rec.Header().Get("Location"), page)
			}
		})
	}

	// The confirm steps are unchanged: without confirm=yes a Datastar post is
	// still a 428, and nothing changes.
	fs := &fakeSafety{perms: safetyPerms()}
	s := safetyServer(fs, nil, SafetyFacts{})
	if rec := sendForm(s.Handler(), http.MethodPost, "/safety/mode", url.Values{"category": {"web"}, "mode": {"allow"}}, datastar); rec.Code != http.StatusPreconditionRequired || len(fs.called()) != 0 {
		t.Errorf("Allow without its step over Datastar: %d, calls %q; want 428 and none", rec.Code, fs.called())
	}
	f := newFakeResearch()
	if rec := sendForm(researchServer(f).Handler(), http.MethodPost, "/research/delete", url.Values{"run": {"r-1"}}, datastar); rec.Code != http.StatusPreconditionRequired || len(f.called()) != 0 {
		t.Errorf("Delete without its step over Datastar: %d, calls %q; want 428 and none", rec.Code, f.called())
	}
}
