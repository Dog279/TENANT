package mcpremote

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"tenant/internal/logs/event/eventtest"
)

// TestOAuthCallbackNoCodeInLogs (TEN-213): the sign-in callback, on its own
// loopback listener, takes the code for its sign-in's state and ignores a
// forged one, and no log record carries a code or a callback's state,
// whether a browser opened or the sign-in link had to be logged instead. The
// link is the authorize request the operator must open (SignInURL), nothing
// the callback received.
func TestOAuthCallbackNoCodeInLogs(t *testing.T) {
	rec := eventtest.Capture(t)
	for _, browser := range []bool{true, false} {
		addr := freePort(t)
		open := func(string) error {
			go func() {
				time.Sleep(50 * time.Millisecond)
				for _, q := range []string{"error=access_denied&state=FORGED-STATE", "code=FORGED-CODE&state=FORGED-STATE", "code=CODE-SENTINEL&state=st8"} {
					if resp, err := http.Get("http://" + addr + "/callback?" + q); err == nil {
						resp.Body.Close()
					}
				}
			}()
			if !browser {
				return errors.New("no browser here")
			}
			return nil
		}
		res, err := newLazyCodeFetcher(addr, open, "mcp:example")(context.Background(), &auth.AuthorizationArgs{URL: "https://as.example/authorize?client_id=c&state=st8"})
		if err != nil || res.Code != "CODE-SENTINEL" || res.State != "st8" {
			t.Fatalf("browser %v: fetch = %+v, %v; want the code for this sign-in's state", browser, res, err)
		}
	}
	links := 0
	for _, e := range rec.Events() {
		line := e.Msg
		for _, f := range e.Data {
			line += " " + f.Key + "=" + f.Text()
		}
		if strings.Contains(line, "CODE-SENTINEL") || strings.Contains(line, "FORGED") {
			t.Errorf("a callback reached the log: %s", line)
		}
		if e.Provider == "mcp" && e.ID == 2001 {
			links++
		}
	}
	if links != 1 {
		t.Errorf("SignInURL records = %d, want 1 (only when no browser opened)", links)
	}
}

func TestStateFromURL(t *testing.T) {
	if got := stateFromURL("https://mcp.atlassian.com/v1/authorize?client_id=x&state=abc123&scope=read"); got != "abc123" {
		t.Errorf("stateFromURL = %q, want abc123", got)
	}
	if got := stateFromURL("not a url with state"); got != "" {
		t.Errorf("missing state should be empty, got %q", got)
	}
}

// TestAwaitCode_LastWins is the regression for the Atlassian two-step / double-
// authorize bug: the first code is superseded; we must redeem the LAST one.
func TestAwaitCode_LastWins(t *testing.T) {
	results := make(chan authResult, 8)
	// Two callbacks for the same state — the second supersedes the first.
	results <- authResult{code: "stale", state: "S"}
	results <- authResult{code: "fresh", state: "S"}

	got, err := awaitCode(context.Background(), results, "S", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.code != "fresh" {
		t.Errorf("awaitCode redeemed %q, want the freshest 'fresh'", got.code)
	}
}

// TestAwaitCode_IgnoresForeignState confirms a callback for a different state is
// ignored (defends against a stray/foreign redirect into our callback).
func TestAwaitCode_IgnoresForeignState(t *testing.T) {
	results := make(chan authResult, 8)
	results <- authResult{code: "foreign", state: "OTHER"}
	results <- authResult{code: "mine", state: "S"}

	got, err := awaitCode(context.Background(), results, "S", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.code != "mine" {
		t.Errorf("awaitCode = %q, want 'mine' (foreign state ignored)", got.code)
	}
}

func TestAwaitCode_ContextCancel(t *testing.T) {
	results := make(chan authResult) // nothing ever arrives
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := awaitCode(ctx, results, "S", 50*time.Millisecond); err == nil {
		t.Error("expected ctx cancellation error when no code arrives")
	}
}
