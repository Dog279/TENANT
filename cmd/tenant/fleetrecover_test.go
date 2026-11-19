package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fleet"
)

// failingTransport fails the test if anything is sent through it.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("tenant fleet recover sent a request to %s", r.URL)
	return nil, errors.New("not sent")
}

// The recovery identity is sent only where nobody on the way can read it:
// to this computer, or over https. Which address it would go to is decided
// before the identity is read and before anything (the token included) is
// sent.
func TestFleetRecoverHubIsLoopbackOrHTTPS(t *testing.T) {
	for base, ok := range map[string]bool{
		"http://127.0.0.1:9770":       true,
		"http://[::1]:9770":           true,
		"http://localhost:9770":       true,
		"https://10.0.0.5:9770":       true,
		"https://master.example:9770": true,
		"http://10.0.0.5:9770":        false,
		"http://192.0.2.7":            false,
		"http://0.0.0.0:9770":         false,
		"http://master.invalid:9770":  false, // a name that does not resolve is not known to be this computer
		"http://":                     false,
		"ftp://127.0.0.1:9770":        false,
	} {
		if err := recoverHubSafe(base); (err == nil) != ok {
			t.Errorf("%s: %v, want allowed %v", base, err, ok)
		}
	}
}

func TestFleetRecoverRefusesAPlainNetworkHub(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	ch := l.enroll("web1", fleet.Reported{})
	l.confirm("web1", ch)
	prevRead, prevTransport := readRecoveryInput, fleetRecoverTransport
	t.Cleanup(func() { readRecoveryInput, fleetRecoverTransport = prevRead, prevTransport })
	read := false
	readRecoveryInput = func() ([]byte, error) { read = true; return []byte("AGE-SECRET-KEY-1TEST\n"), nil }
	fleetRecoverTransport = failingTransport{t}
	for _, hub := range []string{"10.0.0.5:9770", "http://192.0.2.7:9770"} {
		_, err := l.run("recover", "web1", "--hub", hub)
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "in clear text") || !strings.Contains(err.Error(), "on the master itself") ||
			!strings.Contains(err.Error(), strings.TrimPrefix(hub, "http://")) {
			t.Errorf("--hub %s: %v", hub, err)
		}
		if read {
			t.Fatalf("--hub %s: the identity was read before the refusal", hub)
		}
	}
	// Loopback goes on to read the identity and send it (here, to nobody).
	fleetRecoverTransport = nil
	if _, err := l.run("recover", "web1", "--hub", "127.0.0.1:1"); err == nil || !read || !strings.Contains(err.Error(), "no running Tenant answers") {
		t.Fatalf("a loopback hub: read %v, %v", read, err)
	}
}

// init and enroll say which process holds the fleet and where it listens.
// A holder takes the fleet's lock first and records itself a moment later:
// within --wait, the line waits for that record, and only a wait that runs
// out says no more than "another process holds the fleet".
func TestFleetHolderLineWaitsForTheHoldersRecord(t *testing.T) {
	cfg := t.TempDir()
	lock, err := datalock.ExclusiveFile(filepath.Join(cfg, "fleet.holder.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if got := holderLine(cfg, 300*time.Millisecond); got != "another process holds the fleet" {
		t.Fatalf("a holder that never records itself, after the wait: %q", got)
	}
	since := time.Now().UTC().Truncate(time.Second)
	go func() {
		time.Sleep(600 * time.Millisecond)
		b, _ := json.Marshal(fleet.Holder{PID: 4242, Role: "serve", Address: "127.0.0.1:9777", Since: since})
		_ = os.WriteFile(filepath.Join(cfg, "fleet.holder"), b, 0o600)
	}()
	want := "tenant serve (process 4242) holds the fleet, on 127.0.0.1:9777 since " + since.Local().Format(time.DateTime)
	if got := holderLine(cfg, 10*time.Second); got != want {
		t.Fatalf("a holder that records itself late: %q, want %q", got, want)
	}
}
