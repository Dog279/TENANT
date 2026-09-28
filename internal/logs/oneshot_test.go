package logs

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

var oneShotAudit = testProvider.Event(4001, event.Info, "Audit", "Changed {what}", "what")

// A one-shot command's audit record reaches the store, chained; its other
// events keep the no-emitter path (warnings to slog.Default).
func TestAuditEmitterFilesAuditsAndPassesTheRestOn(t *testing.T) {
	root := t.TempDir()
	h, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	path := filepath.Join(root, "logbook.db")
	event.Install(AuditEmitter(h, func() (*store.Store, error) {
		opened++
		return store.Open(path, store.Options{ChainKey: bytes.Repeat([]byte{7}, 32)})
	}))
	defer event.Install(nil)
	var out bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	defer slog.SetDefault(prev)

	testLost.Log(context.Background(), "name", "calendar") // critical: printed, not opened
	if opened != 0 || !strings.Contains(out.String(), "Store calendar lost") {
		t.Fatalf("a plain event opened the store (%d) or missed slog: %q", opened, out.String())
	}
	for range 2 {
		if err := oneShotAudit.Audit(context.Background(), "what", "config"); err != nil {
			t.Fatal(err)
		}
	}
	if opened != 1 {
		t.Fatalf("the store was opened %d times", opened)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, _, _, err := st.Since(context.Background(), store.Cursor{}, 10)
	if err != nil || len(evs) != 2 || evs[0].Msg != "Changed config" {
		t.Fatalf("stored %v (err %v)", evs, err)
	}
}
