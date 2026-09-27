package web

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

type chromeEvents struct {
	mu  sync.Mutex
	got []*event.Event
}

func (c *chromeEvents) Emit(_ context.Context, e *event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, e)
}
func (c *chromeEvents) Audit(ctx context.Context, e *event.Event) error { c.Emit(ctx, e); return nil }

func (c *chromeEvents) find(msg string) *event.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.got {
		if e.Msg == msg {
			return e
		}
	}
	return nil
}

func dataOf(e *event.Event, key string) string {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Text()
		}
	}
	return ""
}

// Chrome's start and exit are filed in the browser log (TEN-364); an exit
// Tenant caused is not a crash. Needs a local Chrome, so it is gated:
//
//	TENANT_CHROME=1 go test ./internal/plugins/web/ -run TestChromeIsFiledInTheBrowserLog -v
func TestChromeIsFiledInTheBrowserLog(t *testing.T) {
	if os.Getenv("TENANT_CHROME") == "" {
		t.Skip("set TENANT_CHROME=1 to start a real Chrome")
	}
	em := &chromeEvents{}
	event.Install(em)
	t.Cleanup(func() { event.Install(nil) })
	sess, err := NewSession(Config{Headless: true})
	if err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	start := em.find("process started")
	if start == nil || start.Log != "browser" || dataOf(start, "child") != "chrome" || dataOf(start, "pid") == "0" {
		t.Fatalf("start line = %+v", start)
	}
	_ = sess.Close()
	deadline := time.Now().Add(10 * time.Second)
	for em.find("process exited: "+errChromeExit.Error()) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	exit := em.find("process exited: " + errChromeExit.Error())
	if exit == nil || exit.Level != event.Info || dataOf(exit, "cancelled") != "true" || dataOf(exit, "duration") == "" {
		t.Fatalf("exit line = %+v", exit)
	}
}
