package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/model"
)

// newTestService spins a Service against a stub httptest server.
// h is the handler the server runs; the service's BaseURL is the
// test server's URL. Token is a bogus literal — we just assert it
// arrives in Authorization headers when checking auth shape.
func newTestService(t *testing.T, h http.HandlerFunc) (*Service, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	svc, err := Open(Config{Token: "test-token-xyz", BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return svc, srv
}

// --- Open() validation ---

// TestOpen_RejectsBlankToken — token is the load-bearing credential;
// an empty/whitespace token must error at Open, not at first call.
// Catches operators who forget --discord-bot-token or have an empty
// $DISCORD_BOT_TOKEN.
func TestOpen_RejectsBlankToken(t *testing.T) {
	for _, tok := range []string{"", "   ", "\t\n"} {
		if _, err := Open(Config{Token: tok}); err == nil {
			t.Errorf("blank token %q should error", tok)
		}
	}
}

// TestOpen_DefaultsToProductionAPI — when BaseURL is unset, the v10
// production API is used. Drift guard against an accidental change
// that points at a staging URL.
func TestOpen_DefaultsToProductionAPI(t *testing.T) {
	svc, err := Open(Config{Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svc.a.base, "discord.com/api/v10") {
		t.Errorf("default base should be v10 production; got %q", svc.a.base)
	}
}

// --- Auth header ---

// TestAuthHeader_BotPrefix — every Discord call MUST send
// "Authorization: Bot <token>" — the literal "Bot " prefix is required
// per discord.com/developers/reference. A previous version that sent
// just the bare token would 401 silently against any real endpoint.
func TestAuthHeader_BotPrefix(t *testing.T) {
	var sawAuth string
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[]`))
	})
	_, _ = svc.ListGuilds(context.Background())
	if sawAuth != "Bot test-token-xyz" {
		t.Errorf("auth header malformed: got %q, want %q", sawAuth, "Bot test-token-xyz")
	}
}

// TestUserAgent_Present — Discord requires a recognizable User-Agent
// header (per their docs). Missing UA = abuse-detector trip.
func TestUserAgent_Present(t *testing.T) {
	var ua string
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[]`))
	})
	_, _ = svc.ListGuilds(context.Background())
	if !strings.HasPrefix(ua, "DiscordBot ") {
		t.Errorf("User-Agent must start with 'DiscordBot '; got %q", ua)
	}
}

// --- ListGuilds happy + sad paths ---

func TestListGuilds_ParsesResponse(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/users/@me/guilds" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"id":"111","name":"Server One"},{"id":"222","name":"Server Two"}]`))
	})
	guilds, err := svc.ListGuilds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(guilds) != 2 || guilds[0].Name != "Server One" {
		t.Errorf("bad parse: %+v", guilds)
	}
}

// --- ListChannels ---

func TestListChannels_RequiresGuildID(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not have made a network call")
	})
	if _, err := svc.ListChannels(context.Background(), ""); err == nil {
		t.Error("blank guild id must error before any HTTP call")
	}
}

func TestListChannels_PathEscapesID(t *testing.T) {
	// Defense against an operator passing a malformed guild ID with
	// reserved URL chars — we must escape, not crash. Note: r.URL.Path
	// is the DECODED path; the on-the-wire form lives in r.URL.RawPath.
	var sawRawPath string
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		sawRawPath = r.URL.RawPath
		if sawRawPath == "" {
			sawRawPath = r.URL.Path // no escapes in URL → RawPath empty
		}
		_, _ = w.Write([]byte(`[]`))
	})
	_, _ = svc.ListChannels(context.Background(), "weird/id")
	if !strings.Contains(sawRawPath, "weird%2Fid") {
		t.Errorf("guild id not URL-escaped on the wire; got raw path %q", sawRawPath)
	}
}

// --- ReadChannel limit clamping ---

func TestReadChannel_LimitClampedTo100(t *testing.T) {
	// Discord's REST cap is 100. We must clamp so the server doesn't
	// 400 on the operator's behalf for a too-large limit.
	var sawLimit string
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		sawLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[]`))
	})
	_, _ = svc.ReadChannel(context.Background(), "1234", 999)
	if sawLimit != "100" {
		t.Errorf("limit not clamped to 100; got %q", sawLimit)
	}
}

func TestReadChannel_LimitDefaultsTo25(t *testing.T) {
	var sawLimit string
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		sawLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[]`))
	})
	_, _ = svc.ReadChannel(context.Background(), "1234", 0)
	if sawLimit != "25" {
		t.Errorf("limit default broken; got %q", sawLimit)
	}
}

// --- SendMessage safety: allowed_mentions ---

// TestSendMessage_AllowedMentionsLockedDown — the load-bearing safety
// test. The plan's §4 commits to "@everyone/role pings auto-blocked."
// This asserts the request body actually carries the lockdown payload
// (parse=[]) — a refactor that drops this line would silently let a
// hallucinating model fire mass pings.
func TestSendMessage_AllowedMentionsLockedDown(t *testing.T) {
	var gotBody map[string]any
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST; got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"99","channel_id":"1234","content":"hi"}`))
	})
	if _, err := svc.SendMessage(context.Background(), "1234", "hi"); err != nil {
		t.Fatal(err)
	}
	am, ok := gotBody["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions missing from body: %+v", gotBody)
	}
	parse, ok := am["parse"].([]any)
	if !ok || len(parse) != 0 {
		t.Errorf("allowed_mentions.parse must be empty array (locked down); got %#v", am["parse"])
	}
}

// --- Error path ---

func TestErrorPath_StructuredDiscordError(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"code":50001,"message":"Missing Access"}`))
	})
	_, err := svc.ListGuilds(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError; got %T", err)
	}
	if ae.Status != 403 || ae.DiscordErr != 50001 {
		t.Errorf("wrong shape: status=%d discordErr=%d msg=%q", ae.Status, ae.DiscordErr, ae.Message)
	}
}

// TestErrorPath_429RetryAfter — 429 with a Retry-After header populates
// RetryAfter on the apiError. The RetryDecorator (internal/agent/retry.go)
// will see "i/o timeout"-style transients but not 429s directly — those
// are surfaced for the caller so the model can decide whether to back off.
func TestErrorPath_429RetryAfter(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1.5")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"code":0,"message":"You are being rate limited."}`))
	})
	_, err := svc.ListGuilds(context.Background())
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError; got %v", err)
	}
	if ae.RetryAfter <= 0 {
		t.Errorf("RetryAfter not populated from header; got %v", ae.RetryAfter)
	}
}

// --- Dispatcher tests ---

// TestDispatcher_ToolNamesStable — drift guard. The tool catalog is
// what the model sees; renames silently break agent skills that
// reference these tools by name. This pins the exact 5 names per the
// shipped plan (Surface A only).
func TestDispatcher_ToolNamesStable(t *testing.T) {
	d := NewDispatcher(nil, Policy{})
	names := map[string]bool{}
	for _, sp := range d.Tools() {
		names[sp.Name] = true
	}
	want := []string{
		"discord_list_guilds",
		"discord_list_channels",
		"discord_read_channel",
		"discord_send_message",
		"discord_react",
	}
	for _, n := range want {
		if !names[n] {
			t.Errorf("tool %q missing from catalog (renamed?)", n)
		}
	}
	if len(names) != 5 {
		t.Errorf("expected exactly 5 tools (Surface A only); got %d: %v", len(names), names)
	}
}

// TestDispatcher_NilServiceServesTools — stub catalog usage at
// toolmux.go:763 calls NewDispatcher(nil, Policy{}).Tools(). Must not
// panic and must return the same spec list as the configured dispatcher.
func TestDispatcher_NilServiceServesTools(t *testing.T) {
	d := NewDispatcher(nil, Policy{})
	specs := d.Tools()
	if len(specs) != 5 {
		t.Errorf("nil-service Tools() should return 5 specs; got %d", len(specs))
	}
	// And Dispatch with no service returns "not configured" — never panics.
	r, isErr, err := d.Dispatch(context.Background(), model.ToolCall{Name: "discord_list_guilds"})
	if err != nil || !isErr || !strings.Contains(r, "not configured") {
		t.Errorf("nil-service dispatch shape wrong: r=%q isErr=%v err=%v", r, isErr, err)
	}
}

// TestDispatcher_UnknownToolName — defensive return for a typo or
// stale tool name (e.g. the model invents discord_search which we
// intentionally don't ship).
func TestDispatcher_UnknownToolName(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unknown tool must not hit the network")
	})
	d := NewDispatcher(svc, Policy{})
	r, isErr, err := d.Dispatch(context.Background(), model.ToolCall{Name: "discord_search"})
	if err != nil || !isErr || !strings.Contains(r, "unknown") {
		t.Errorf("unknown-tool shape wrong: r=%q isErr=%v err=%v", r, isErr, err)
	}
}

// TestPolicy_GatesSendWithoutAllow — load-bearing safety guard.
// Without AllowSend AND without a Confirm callback, discord_send_message
// MUST be rejected. A regression here means the model can post publicly
// without operator consent.
func TestPolicy_GatesSendWithoutAllow(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("gated send must NEVER reach the network")
	})
	d := NewDispatcher(svc, Policy{AllowSend: false, Confirm: nil})
	r, isErr, err := d.Dispatch(context.Background(), model.ToolCall{
		Name:      "discord_send_message",
		Arguments: json.RawMessage(`{"channel_id":"1","content":"x"}`),
	})
	if err != nil || !isErr || !strings.Contains(r, "blocked") {
		t.Errorf("expected blocked-by-policy result; got r=%q isErr=%v err=%v", r, isErr, err)
	}
}

// TestPolicy_AllowSendPermitsSend — when AllowSend=true, send goes through.
func TestPolicy_AllowSendPermitsSend(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"99","channel_id":"1","content":"x"}`))
	})
	d := NewDispatcher(svc, Policy{AllowSend: true})
	r, isErr, err := d.Dispatch(context.Background(), model.ToolCall{
		Name:      "discord_send_message",
		Arguments: json.RawMessage(`{"channel_id":"1","content":"x"}`),
	})
	if err != nil || isErr {
		t.Errorf("AllowSend=true should permit send: r=%q isErr=%v err=%v", r, isErr, err)
	}
}

// TestPolicy_ConfirmCallbackCanGrant — operator-confirmed send goes
// through even when AllowSend=false. Used by the interactive TUI
// approval flow.
func TestPolicy_ConfirmCallbackCanGrant(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"99","channel_id":"1","content":"x"}`))
	})
	var sawAction, sawDetail string
	confirm := func(_ context.Context, action, detail string) bool {
		sawAction, sawDetail = action, detail
		return true
	}
	d := NewDispatcher(svc, Policy{AllowSend: false, Confirm: confirm})
	r, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{
		Name:      "discord_send_message",
		Arguments: json.RawMessage(`{"channel_id":"abc","content":"hello"}`),
	})
	if isErr {
		t.Errorf("Confirm=true should permit send; got %q", r)
	}
	if !strings.Contains(sawAction, "Discord") {
		t.Errorf("Confirm received wrong action label: %q", sawAction)
	}
	if !strings.Contains(sawDetail, "abc") || !strings.Contains(sawDetail, "hello") {
		t.Errorf("Confirm detail should preview channel + content; got %q", sawDetail)
	}
}
