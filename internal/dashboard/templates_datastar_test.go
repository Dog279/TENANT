package dashboard

// Datastar v1.0.1 binds COLON attribute syntax (data-on:click) and SILENTLY
// ignores the hyphen forms (data-on-click) — exactly what left the dashboard
// inert after TEN-108/109 until the TEN-110 QA fix, and a failure mode no
// server-side httptest catches because the markup renders fine; only a browser
// notices nothing is bound. TestSSR_DatastarColonSyntax spot-checks three
// rendered pages; this scans EVERY embedded template so a newly-added page can't
// reintroduce a dead binding (the "dashboard stopped updating" class of bug).

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestTemplates_NoDeadHyphenDatastarAttrs(t *testing.T) {
	// Match a Datastar plugin attribute written with the dead hyphen separator
	// (data-on-..., data-bind-..., etc.). The live colon form (data-on:...) and
	// keyless plugins (data-init, data-bind:text) do not match, and neither do
	// the plugins whose own names are hyphenated (data-on-intersect).
	dead := regexp.MustCompile(`data-(on|bind|text|show|class|attr|computed)-[a-z]+`)
	live := map[string]bool{"data-on-intersect": true, "data-on-interval": true, "data-on-signal": true}
	walked := 0
	err := fs.WalkDir(ssrFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		walked++
		b, rerr := ssrFS.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range dead.FindAllString(string(b), -1) {
			if live[m] {
				continue
			}
			t.Errorf("%s uses dead hyphen-form Datastar attr %q — Datastar v1.0.1 silently ignores it; use the colon form (e.g. data-on:click)", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked == 0 {
		t.Fatal("no templates scanned — embed FS path wrong?")
	}
}

// The dashboard live-feed depends on a single long-lived SSE opened by the
// browser via data-init="@get('/events')". Datastar auto-retries a dropped SSE
// but GIVES UP PERMANENTLY after retryMaxCount (default 10) — so a tab left open
// across a few `tenant` restarts goes dead ("stops updating") until a manual
// reload. A self-hosted dashboard the operator restarts often must reconnect
// indefinitely; this guards that every /events opener raises the cap.
// liveOpener is what a stream opener with a paused banner carries (§2.7).
const liveOpener = `data-signals:_live="true" data-on:datastar-fetch="evt.detail.el === el && ($_live = !['error', 'retrying', 'retries-failed', 'finished'].includes(evt.detail.type))"`

func TestTemplates_SSEReconnectsIndefinitely(t *testing.T) {
	// Each opener → the SSE endpoint it must keep reconnecting to. Activity moved
	// to the retained /activity/events stream (TEN-238); chat stays on /events.
	openers := map[string]string{
		"templates/activity.html": "@get('{{.StreamURL}}'", // /activity/events, query-escaped (F1)
		"templates/chat.html":     "@get('/events'",
		"templates/logs.html":     "@get('{{.TailURL}}'", // the Logs page's live tail
		"templates/safety.html":   "@get('/events?only=approvals'",
		"templates/research.html": "@get('/research/live'",
	}
	for name, ep := range openers {
		b, err := ssrFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(b)
		if !strings.Contains(body, ep) {
			t.Errorf("%s no longer opens its SSE (%s)", name, ep)
		}
		if !strings.Contains(body, "retryMaxCount:1000000") {
			t.Errorf("%s opens an SSE without an unbounded reconnect cap — a dropped SSE (e.g. after a server restart) will stop updating permanently:\n%s", name, body)
		}
	}
	// TEN-238: the activity feed must also keep streaming while the tab is hidden
	// (the macOS background-tab bug) — Datastar aborts the SSE on visibilitychange
	// unless openWhenHidden is set.
	for _, name := range []string{"templates/activity.html", "templates/research.html"} {
		b, _ := ssrFS.ReadFile(name)
		if !strings.Contains(string(b), "openWhenHidden:true") {
			t.Errorf("%s must set openWhenHidden:true so it keeps updating in a backgrounded tab", name)
		}
	}
	// The new openers say when they're down (§2.7): the stream's own fetch
	// events drive the "Live updates paused" banner, which starts hidden. Not
	// data-indicator: it stays on through every retry of a dropped stream.
	for _, name := range []string{"templates/safety.html", "templates/research.html"} {
		b, _ := ssrFS.ReadFile(name)
		for _, want := range []string{liveOpener, `<p class="banner warn live-off" style="display:none" data-show="$_live === false">`} {
			if !strings.Contains(string(b), want) || strings.Contains(string(b), "data-indicator:_live") {
				t.Errorf("%s: the opener needs %s, and no data-indicator:_live", name, want)
			}
		}
	}
}
