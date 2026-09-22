// A 30-line sidecar that logs into Tenant's log book. Build and run it on the
// box next to a running hub (or without one — lines then go to the file):
//
//	go run ./reference/logging/example
//
// In your own program, copy ../tenantlog.go beside your main.go, change its
// first line to `package main`, and drop the import below.
package main

import (
	"log/slog"
	"os"
	"time"

	tenantlog "tenant/reference/logging"
)

func main() {
	log := tenantlog.New("example-sidecar") // → <data>/logs/example-sidecar/ + an "example-sidecar" tab

	if err := log.Ping(); err != nil {
		log.Warn("no live hub — lines go to the day's file", "reason", err)
	}
	log.Info("started", "pid", os.Getpid(), "version", "0.1")

	// Optional: route the program's existing slog output too.
	slog.SetDefault(slog.New(log.Handler()))
	slog.Info("this line came through slog", "elapsed", 250*time.Millisecond)

	// A dashboard-styled event line (badge = the kind).
	_ = log.Event("tool_call", "tool call disk_check", map[string]string{"tool": "disk_check", "args": `{"path":"/"}`})

	log.Error("something failed", "err", os.ErrNotExist)
}
