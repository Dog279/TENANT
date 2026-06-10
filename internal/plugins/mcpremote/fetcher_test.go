package mcpremote

import (
	"context"
	"testing"
	"time"
)

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
