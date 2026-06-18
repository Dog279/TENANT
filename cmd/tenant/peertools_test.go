package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
	"tenant/internal/peering"
	"tenant/internal/plugins/mcpremote"
)

// TestPeerKnowledge_EndToEnd is the TEN-186 acceptance demo, hermetic: a paired
// peer cross-queries shared memory and gets the SHARED fact WITH provenance but
// NEVER the private one; and after the serving side flips memory=off, the same
// query is DENIED at call time (no reconnect). This exercises the full client
// (OpenStatic) ↔ server (peerKnowledgeRegistrar on the real listener) loop.
func TestPeerKnowledge_EndToEnd(t *testing.T) {
	ctx := context.Background()

	// --- serving side: seed a shared fact + a private fact ---
	dir := t.TempDir()
	ss, err := semantic.Open(dir + "/facts.db")
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	mustInsert(t, ss, &semantic.Fact{AgentID: "main", EmbedderID: "test/0", Fact: "the zebra deployment runbook lives in the ops vault", Visibility: semantic.VisibilityShared, Confidence: 0.9, Embedding: []float32{0.1, 0.2, 0.3}})
	mustInsert(t, ss, &semantic.Fact{AgentID: "main", EmbedderID: "test/0", Fact: "the zebra root password is hunter2", Visibility: semantic.VisibilityPrivate, Confidence: 0.9, Embedding: []float32{0.1, 0.2, 0.3}})

	store, _ := peering.LoadStore(dir)
	store.CreateInvite("hub", "hub-id", "https://hub", "", time.Hour, "spoke")
	store.SetShare("spoke", "memory", true)
	p, _ := store.Get("spoke")

	deps := peerToolDeps{selfName: "hubtest", semantic: ss} // no embedder → keyword-only; no episodic/wiki
	l, _ := peering.NewListener(peering.ListenerConfig{
		Store: store, SelfID: "hub-id", SelfVersion: "test",
		Registrar: peerKnowledgeRegistrar(deps),
	})
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()

	// --- dialing side: connect with the static pairing token ---
	d, cleanup, err := mcpremote.OpenStatic(ctx, mcpremote.StaticConfig{
		ServerURL: srv.URL, Token: p.Token, Label: "peer:hub",
		UngateTools: peerFederationTools, // TEN-252: known federation tools must stay ungated
	}, mcpremote.Policy{})
	if err != nil {
		t.Fatalf("OpenStatic: %v", err)
	}
	defer cleanup()

	// Query shared memory: gets the SHARED fact, never the PRIVATE one.
	out, isErr, err := d.Dispatch(ctx, model.ToolCall{Name: "peer_memory_search", Arguments: []byte(`{"query":"zebra"}`)})
	if err != nil || isErr {
		t.Fatalf("peer_memory_search errored: isErr=%v err=%v out=%q", isErr, err, out)
	}
	if !strings.Contains(out, "runbook") {
		t.Errorf("shared fact should be returned; got %q", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(strings.ToLower(out), "root password") {
		t.Fatalf("PRIVATE memory leaked across the peer boundary: %q", out)
	}
	if !strings.Contains(out, "hubtest") {
		t.Errorf("result should carry provenance (serving instance name); got %q", out)
	}

	// Flip memory off → the SAME query is denied at call time (no reconnect).
	store.SetShare("spoke", "memory", false)
	out2, isErr2, _ := d.Dispatch(ctx, model.ToolCall{Name: "peer_memory_search", Arguments: []byte(`{"query":"zebra"}`)})
	if !isErr2 || !strings.Contains(strings.ToLower(out2), "denied") {
		t.Errorf("after memory=off the query must be DENIED at call time; got isErr=%v out=%q", isErr2, out2)
	}
}

func mustInsert(t *testing.T, ss *semantic.Store, f *semantic.Fact) {
	t.Helper()
	if _, err := ss.Insert(context.Background(), f); err != nil {
		t.Fatalf("seed fact: %v", err)
	}
}
