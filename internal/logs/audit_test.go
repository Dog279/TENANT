package logs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// A Security record that can't be written is a critical system event and
// one banner; under deny, Gate refuses the action it records. The next
// record written ends the outage: a system/4002 and a second banner.
func TestAuditFailureReportedUntilRecovered(t *testing.T) {
	var notices []string
	h, root, _ := openHub(t, Options{Notice: func(s string) { notices = append(notices, s) }, AuditDeny: func() bool { return true }})
	keyless, err := store.Open(filepath.Join(root, "keyless.db"), store.Options{}) // refuses security records
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(keyless)
	Install(h)
	defer Install(nil)
	ctx := context.Background()

	for range 2 {
		if err := catalog.SignedIn.Gate(ctx, "remote", "127.0.0.1:1"); !errors.Is(err, event.ErrRefused) || !errors.Is(err, store.ErrNoChainKey) {
			t.Fatalf("Gate with the record refused under deny = %v", err)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("notices while failing: %q, want one banner", notices)
	}

	st := attach(t, h) // a store with its key
	if err := catalog.SignedIn.Gate(ctx, "remote", "127.0.0.1:1"); err != nil {
		t.Fatalf("Gate once records are written again = %v", err)
	}
	if len(notices) != 2 {
		t.Fatalf("notices after recovery: %q", notices)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var recovered bool
		for _, e := range all(t, st) {
			recovered = recovered || e.Provider == "system" && e.ID == 4002 && e.Data[0].Text() == "2"
		}
		if recovered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no system/4002 counting the two lost records")
		}
	}
}
