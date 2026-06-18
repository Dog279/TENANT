package peering

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
	"tenant/internal/plugins/mcpremote"
)

// TestPeerHello_StructuredCapsRoundTrip proves the TEN-251 path end-to-end
// against the REAL go-sdk + listener: peer_hello's typed result (whose
// "capabilities" is the grant the serving side gives the caller) survives
// mcpremote.Dispatcher.CallRawJSON and parses — i.e. the /peer status
// "them → we" column will actually populate. Dispatch() (text only) would NOT
// have surfaced it, since the result lives in StructuredContent.
func TestPeerHello_StructuredCapsRoundTrip(t *testing.T) {
	srv, token, store := pairedListener(t, nil)
	defer srv.Close()
	// Grant the spoke wiki+memory so the stamp's capabilities is non-empty.
	if err := store.SetShare("spoke", "wiki", true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetShare("spoke", "memory", true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, cleanup, err := mcpremote.OpenStatic(ctx, mcpremote.StaticConfig{
		ServerURL: srv.URL, Token: token, Label: "probe", TLS: nil, // httptest = plain HTTP
	}, mcpremote.Policy{})
	if err != nil {
		t.Fatalf("OpenStatic: %v", err)
	}
	defer cleanup()

	raw, err := d.CallRawJSON(ctx, model.ToolCall{Name: "peer_hello", Arguments: []byte("{}")})
	if err != nil {
		t.Fatalf("CallRawJSON(peer_hello): %v", err)
	}
	var h struct {
		InstanceID   string   `json:"instance_id"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("peer_hello result is not parseable JSON: %v (raw=%s)", err, raw)
	}
	if h.InstanceID != "hub-id" {
		t.Errorf("instance_id = %q, want hub-id", h.InstanceID)
	}
	got := strings.Join(h.Capabilities, ",")
	if !strings.Contains(got, "wiki") || !strings.Contains(got, "memory") {
		t.Fatalf("capabilities = %q, want wiki+memory (the grant to the caller)", got)
	}
}

// TestPeerHello_GrantAnnounce proves the TEN-253 symmetric exchange end-to-end:
// a dialer announces its OWN grant in the peer_hello input, and the listener's
// OnGrant hook receives it (the acceptor-side source for "them → we").
func TestPeerHello_GrantAnnounce(t *testing.T) {
	dir := t.TempDir()
	store, _ := LoadStore(dir)
	if _, err := store.CreateInvite("hub", "hub-id", "https://hub", "", time.Hour, "spoke"); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Get("spoke")

	var gotName string
	var gotGrant []string
	l, err := NewListener(ListenerConfig{
		Store: store, SelfID: "hub-id", SelfVersion: "1.0",
		OnGrant: func(name string, grant []string) { gotName = name; gotGrant = grant },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, cleanup, err := mcpremote.OpenStatic(ctx, mcpremote.StaticConfig{
		ServerURL: srv.URL, Token: p.Token, Label: "probe", TLS: nil,
	}, mcpremote.Policy{})
	if err != nil {
		t.Fatalf("OpenStatic: %v", err)
	}
	defer cleanup()

	arg, _ := json.Marshal(map[string][]string{"grant": {"wiki", "memory"}})
	if _, err := d.CallRawJSON(ctx, model.ToolCall{Name: "peer_hello", Arguments: arg}); err != nil {
		t.Fatalf("CallRawJSON(peer_hello+grant): %v", err)
	}
	if gotName != "spoke" || strings.Join(gotGrant, ",") != "wiki,memory" {
		t.Fatalf("OnGrant got (%q, %v), want (spoke, [wiki memory])", gotName, gotGrant)
	}
}
