package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// OpenDM creates (or fetches) the bot↔user DM channel via POST
// /users/@me/channels {recipient_id} and returns its id — the primitive that
// lets Tenant reach the operator when no turn is in flight.
func TestOpenDM(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "dm-123", "type": 1})
	}))
	defer srv.Close()
	svc, err := Open(Config{Token: "t", BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	id, err := svc.OpenDM(context.Background(), "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "dm-123" {
		t.Errorf("channel id = %q, want dm-123", id)
	}
	if gotPath != "POST /users/@me/channels" || gotBody != `{"recipient_id":"op-1"}` {
		t.Errorf("wire = %s %s", gotPath, gotBody)
	}
	if _, err := svc.OpenDM(context.Background(), " "); err == nil {
		t.Error("an empty user id must be rejected before any request")
	}
}
