package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSimpleFINBridge serves a claimable token + a 2-account book.
func fakeSimpleFINBridge(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/claim/TOK":
			fmt.Fprintf(w, "http://u:p@%s/simplefin", srv.Listener.Addr().String())
		case r.Method == http.MethodGet && r.URL.Path == "/simplefin/accounts":
			fmt.Fprint(w, `{"errors":[],"accounts":[
				{"org":{"name":"First Bank"},"id":"a1","name":"Checking","currency":"USD","balance":"10.00","balance-date":1755000000,"transactions":[]},
				{"org":{"name":"Broker Co"},"id":"a2","name":"Brokerage","currency":"USD","balance":"20.00","balance-date":1755000000,"transactions":[]}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSimpleFINProbe_ClaimsTokenAndPersists: the probe must swap a
// stored setup token for the claimed access URL via the save callback
// (tokens are one-shot at the bridge), then verify with a live fetch.
func TestSimpleFINProbe_ClaimsTokenAndPersists(t *testing.T) {
	srv := fakeSimpleFINBridge(t)
	tok := base64.StdEncoding.EncodeToString([]byte(srv.URL + "/claim/TOK"))
	creds := &credentials{Secrets: map[string]string{
		skillSecretID("simplefin", "access_url"): tok,
	}}
	saved := 0
	identity, err := probeSimpleFINWith(context.Background(), creds, func() error { saved++; return nil }, srv.Client())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if saved != 1 {
		t.Errorf("save called %d times, want 1 (claim must persist immediately)", saved)
	}
	stored := creds.Get(skillSecretID("simplefin", "access_url"))
	if !strings.Contains(stored, "u:p@") || !strings.HasPrefix(stored, "http") {
		t.Errorf("stored secret not rewritten to the access URL: %q", stored)
	}
	if !strings.Contains(identity, "2 account(s) across 2 institution(s)") {
		t.Errorf("identity = %q", identity)
	}

	// Second probe with the (already-claimed) access URL: no re-claim, no save.
	saved = 0
	if _, err := probeSimpleFINWith(context.Background(), creds, func() error { saved++; return nil }, srv.Client()); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if saved != 0 {
		t.Error("second probe must not re-claim or re-save")
	}
}

// TestSimpleFINRegistered guards the catalog + keys-page entries.
func TestSimpleFINRegistered(t *testing.T) {
	k, ok := skillKinds["simplefin"]
	if !ok {
		t.Fatal("skillKinds[simplefin] not registered")
	}
	if !k.Wired || len(k.Fields) != 1 || !k.Fields[0].Secret || k.Fields[0].Key != "access_url" {
		t.Errorf("simplefin kind mis-shaped: %+v", k)
	}
	if _, ok := lookupKeySpec("skill:simplefin:access_url"); !ok {
		t.Error("keys catalog is missing skill:simplefin:access_url")
	}
}
