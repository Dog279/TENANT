package peering

import (
	"context"
	"encoding/json"
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
