package web

import (
	"os"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
)

func find(c *eventtest.Recorder, msg string) *event.Event {
	for _, e := range c.Events() {
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
	em := eventtest.Capture(t)
	sess, err := NewSession(Config{Headless: true})
	if err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	start := find(em, "process started")
	if start == nil || start.Log != "browser" || dataOf(start, "child") != "chrome" || dataOf(start, "pid") == "0" {
		t.Fatalf("start line = %+v", start)
	}
	_ = sess.Close()
	deadline := time.Now().Add(10 * time.Second)
	for find(em, "process exited: "+errChromeExit.Error()) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	exit := find(em, "process exited: "+errChromeExit.Error())
	if exit == nil || exit.Level != event.Info || dataOf(exit, "cancelled") != "true" || dataOf(exit, "duration") == "" {
		t.Fatalf("exit line = %+v", exit)
	}
}
