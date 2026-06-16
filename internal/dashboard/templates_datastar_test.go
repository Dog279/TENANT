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
	// keyless plugins (data-init, data-bind:text) do not match.
	dead := regexp.MustCompile(`data-(on|bind|text|show|class|attr|computed)-[a-z]`)
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
		if m := dead.FindString(string(b)); m != "" {
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
func TestTemplates_SSEReconnectsIndefinitely(t *testing.T) {
	// Each opener → the SSE endpoint it must keep reconnecting to. Activity moved
	// to the retained /activity/events stream (TEN-238); chat stays on /events.
	openers := map[string]string{
		"templates/activity.html": "@get('/activity/events",
		"templates/chat.html":     "@get('/events'",
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
	b, _ := ssrFS.ReadFile("templates/activity.html")
	if !strings.Contains(string(b), "openWhenHidden:true") {
		t.Error("activity.html must set openWhenHidden:true so the feed keeps updating in a backgrounded tab")
	}
}
