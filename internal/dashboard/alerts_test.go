package dashboard

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The Overview's health card says how many alerts fired in the last day,
// whichever process filed them, and links to them; with none, healthy.
func TestHomeAlertCard(t *testing.T) {
	root := t.TempDir()
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	t.Cleanup(func() { _ = h.Close() })
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(h)
	if body := get(t, s, "/").Body.String(); !strings.Contains(body, "healthy") || strings.Contains(body, "log:alerts") { // every page has role="alert" (#toast-fail)
		t.Fatalf("no alerts:\n%s", body)
	}
	other, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{}) // another process's
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	now := time.Now().UTC()
	alert := func(at time.Time, msg string) event.Event {
		return event.Event{Log: "alerts", Provider: "alerts", ID: 1001, Level: event.Warning, Time: at, Msg: msg}
	}
	if err := other.Commit(context.Background(), []event.Event{alert(now.Add(-48*time.Hour), "old"),
		alert(now.Add(-time.Hour), "Alert critical: system/4 critical: first"), alert(now, "Alert critical: system/4 critical: the newest")}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/").Body.String()
	for _, want := range []string{`<span class="trend-down">2 alerts</span>`, `href="/logs?q=log:alerts"`, `title="Alert critical: system/4 critical: the newest"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home missing %q:\n%s", want, body)
		}
	}
}

// UX-8: a model call that failed in the last hour turns the Agent card from
// "healthy" to "Can't reach its model", linking to the model's log; one
// older than that doesn't.
func TestHomeModelTroubleCard(t *testing.T) {
	root := t.TempDir()
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	t.Cleanup(func() { _ = h.Close() })
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(h)
	down := func(at time.Time) event.Event {
		return event.Event{Log: "model", Provider: "model", ID: 3001, Level: event.Error, Time: at, Msg: "Model main unavailable (endpoint down)"}
	}
	if err := st.Commit(context.Background(), []event.Event{down(time.Now().UTC().Add(-2 * time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if body := get(t, s, "/").Body.String(); !strings.Contains(body, "healthy") {
		t.Errorf("a failure two hours ago still shows:\n%s", body)
	}
	if err := st.Commit(context.Background(), []event.Event{down(time.Now().UTC())}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, "healthy") || !strings.Contains(body, "Can't reach its model") || !strings.Contains(body, `href="/logs?q=log:model"`) {
		t.Errorf("a model that just failed:\n%s", body)
	}
}
