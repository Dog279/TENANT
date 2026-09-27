package main

import (
	"runtime/debug"
	"sync/atomic"

	"tenant/internal/lifecycle"
	"tenant/internal/logs"
)

// startBootRecord ties the log book to the process's lifetime (docs/design/
// logging-v2.md §12.2), for serve and the TUI only: a one-shot command keeps
// no boot record, so a Ctrl-C never reads as a crash. The log book is the
// first thing opened and the last released; call this right after the
// lifecycle group is made:
//
//   - the log book's final flush, bounded at 2 s, is the first Release step
//     registered, so it runs last: what every other stop logged is on disk.
//     The store stays open for the shutdown's own last events (its end, and
//     StoppedOK after it) until the caller's deferred hub.Close, registered
//     right after the hub opened, which flushes again and closes it;
//   - the boot record and crash capture start (logs.Hub.BeginBoot, which
//     also files what earlier runs left behind), and their clean stop is the
//     next Release step, so it runs just before the flush.
//
// Defer the returned function. A panic on the calling goroutine runs the
// deferred shutdown before the runtime prints the crash; onPanic sees it
// first, so that shutdown neither marks the record clean nor turns the crash
// file off.
func startBootRecord(hub *logs.Hub, life *lifecycle.Group, role string) (onPanic func()) {
	var crashed atomic.Bool
	onPanic = func() {
		if r := recover(); r != nil {
			crashed.Store(true)
			panic(r)
		}
	}
	debug.SetTraceback("all") // an unrecovered panic dumps every goroutine, not only its own
	life.Add(lifecycle.Release, "log book", hub.Flush)
	if stop := hub.BeginBoot(role); stop != nil {
		life.AddFunc(lifecycle.Release, "boot record", func() {
			if !crashed.Load() {
				stop()
			}
		})
	}
	return onPanic
}
