package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/lifecycle"
	"tenant/internal/logs"
)

// Every subsystem serve starts is listed with its state (TEN-380): serve on
// the echo backend, with its defaults and the dashboard on, lists them all
// running, and each one it stops is stopped once serve has shut down.
func TestServeListsItsComponents(t *testing.T) {
	if testing.Short() {
		t.Skip("boots serve")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev); logs.Install(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- cmdServe(ctx, []string{"--backend", "echo", "--data", filepath.Join(root, "data"),
			"--config", filepath.Join(root, "cfg"), "--dashboard-addr", "127.0.0.1:0"})
	}()

	want := map[string]lifecycle.State{ // name → its state once serve stopped
		"cron": lifecycle.Stopped, "improve scheduler": lifecycle.Stopped, "dashboard": lifecycle.Stopped,
		"settings watcher": lifecycle.Stopped, "credentials watcher": lifecycle.Stopped,
		"peer heartbeat": lifecycle.Stopped, "turn gate": lifecycle.Running,
	}
	states := func() map[string]lifecycle.State {
		m := map[string]lifecycle.State{}
		for _, c := range lifecycle.Components() {
			m[c.Name] = c.State
		}
		return m
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		got, missing := states(), ""
		for name := range want {
			if got[name] != lifecycle.Running {
				missing = name
			}
		}
		if missing == "" {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("serve returned early (%v); components %v", err, got)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not running after 60 s; components %v", missing, got)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not stop")
	}
	for name, st := range want {
		deadline := time.Now().Add(5 * time.Second) // the watchers stop on their own goroutines
		for states()[name] != st && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := states()[name]; got != st {
			t.Errorf("%s is %s after serve stopped, want %s", name, got, st)
		}
	}
}
