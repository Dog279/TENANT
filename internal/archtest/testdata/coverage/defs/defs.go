// Package defs is a fixture for countSites: it is parsed, never built.
package defs

import (
	"context"

	"tenant/internal/logs/event"
)

var P = event.NewProvider("fixture", "fixture")

var (
	Failed = P.Event(3001, event.Error, "Run", "Run failed: {err}", "err").
		Doc("m", "c", "a")
	Done = P.Event(1001, event.Info, "Run", "Run done")
)

var Denied = P.Event(4103, event.Warning, "Approval", "Denied").Keywords("audit.failure")

func Local(ctx context.Context) {
	Failed.Log(ctx, "err", "x") // with an ID
	Done.Log(ctx)               // info: not counted
}
