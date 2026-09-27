// Package use is a fixture for countSites: it is parsed, never built.
package use

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"tenant/defs"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
)

var log = logs.For("fixture")

func Run(ctx context.Context, w http.ResponseWriter, hub *logs.Hub, err error) {
	defs.Failed.Log(ctx, "err", err)        // with an ID
	_ = defs.Denied.Audit(ctx)              // with an ID
	ctx, act := event.Start(ctx, defs.Done) // with an ID: its fail event is an error
	defer act.End(&err)

	log.Warn("slow")                                     // without
	log.ErrorContext(ctx, "broken", "err", err)          // without
	slog.Error("broken")                                 // without
	log.Log(ctx, slog.LevelError+4, "crashed")           // without
	hub.Logf("fixture", slog.LevelWarn, "odd", "n", 1)   // without
	slog.Default().LogAttrs(ctx, slog.LevelWarn, "odd")  // without
	defs.Done.Log(ctx)                                   // info
	log.Info("fine")                                     // info
	log.Log(ctx, slog.LevelInfo, "fine")                 // info
	hub.Logf("fixture", slog.LevelInfo, "fine")          // info
	http.Error(w, err.Error(), http.StatusInternalError) // not logging
	_ = errors.New("x").Error()                          // not logging
}
