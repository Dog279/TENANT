package mcpremote

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// A dead refresh grant is recorded ONCE (persisted, announced on the
// transition only), readable offline, and cleared — with a second announcement
// — when a valid token is seen again. Before this, the only trace was one
// serve.log Warn per failing call.
func TestReauthState_PersistedAndAnnouncedOnce(t *testing.T) {
	dir := t.TempDir()
	const srv = "https://x/mcp"
	var events []string
	h := newPersistentHandler(srv, "http://127.0.0.1:1/callback", "t", tokenCachePath(dir, srv), false, nil, nil, nil)
	h.onAuthChange = func(needs bool, err string) { events = append(events, fmt.Sprintf("%v:%s", needs, err)) }

	h.markReauth("invalid_grant")
	h.markReauth("invalid_grant") // every later failing call: no second announcement
	got := h.Health()
	if !got.NeedsReauth || got.Err != "invalid_grant" || got.Since.IsZero() {
		t.Fatalf("Health after markReauth = %+v", got)
	}
	if rh, ok := ReadHealth(dir, srv); !ok || !rh.NeedsReauth || rh.Err != "invalid_grant" {
		t.Fatalf("the state must be readable offline from the cache: ok=%v %+v", ok, rh)
	}

	h.clearReauth()
	h.clearReauth() // idempotent
	if h.Health().NeedsReauth {
		t.Fatal("clearReauth must reset the state")
	}
	if rh, _ := ReadHealth(dir, srv); rh.NeedsReauth {
		t.Fatal("the clear must be persisted too")
	}
	if len(events) != 2 || events[0] != "true:invalid_grant" || events[1] != "false:" {
		t.Fatalf("want exactly one dead + one recovered announcement, got %v", events)
	}
}

// A valid token in hand (e.g. right after a browser re-auth, before any
// refresh runs) clears the flag on the hot path — otherwise the status pages
// would keep saying "needs re-auth" for an hour after the operator fixed it.
func TestRefreshingSource_ValidTokenClearsReauth(t *testing.T) {
	dir := t.TempDir()
	const srv = "https://x/mcp"
	h := newPersistentHandler(srv, "", "t", tokenCachePath(dir, srv), false, nil, nil, nil)
	h.markReauth("boom")
	s := &refreshingSource{ctx: context.Background(), h: h, cur: &oauth2.Token{AccessToken: "ok", Expiry: time.Now().Add(2 * time.Hour)}}
	if _, err := s.Token(); err != nil {
		t.Fatal(err)
	}
	if h.Health().NeedsReauth {
		t.Fatal("a valid token must clear the re-auth flag")
	}
}

// The tool error the model sees names the condition and the exact fix.
func TestDispatcher_ReauthTextNamesTheFix(t *testing.T) {
	h := newPersistentHandler("https://mcp.atlassian.com/v1/mcp", "", "t", "", false, nil, nil, nil)
	h.cache = &tokenCache{ReauthNeeded: true, ReauthError: "invalid_grant", ReauthSince: 1_700_000_000}
	h.loaded = true
	d := &Dispatcher{label: "mcp:atlassian", auth: h}
	txt := d.reauthText()
	for _, want := range []string{"mcp:atlassian", "NOT AUTHORIZED", "tenant mcp connect https://mcp.atlassian.com/v1/mcp", "do not retry"} {
		if !strings.Contains(txt, want) {
			t.Errorf("reauth text missing %q: %s", want, txt)
		}
	}
	if (&Dispatcher{label: "peer"}).AuthHealth().NeedsReauth {
		t.Error("a connector without OAuth must report healthy")
	}
}
