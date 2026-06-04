package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// reconnectMonitor implements tui.ReconnectControl: when generation goes
// unreachable, it probes the active backend on a cascading schedule —
// 5 attempts at 1-minute intervals, then every 5 minutes — until the endpoint
// answers, reporting each step to the activity feed. Idempotent: a second
// OnGenerationDown while already retrying is a no-op.
type reconnectMonitor struct {
	cfgDir string
	feed   chan<- string   // the System feed channel (TUI renders it)
	ctx    context.Context // app context; cancels the loop on exit
	log    *slog.Logger
	// fastEvery/slowEvery override the default cadence (0 = default). Tests
	// set these to milliseconds; production leaves them zero.
	fastEvery, slowEvery time.Duration
	running              atomic.Bool
}

const (
	reconnectFastTries = 5
	reconnectFastEvery = time.Minute
	reconnectSlowEvery = 5 * time.Minute
)

// OnGenerationDown starts the reconnect loop if one isn't already running.
func (rm *reconnectMonitor) OnGenerationDown() {
	if rm == nil || rm.feed == nil {
		return
	}
	if !rm.running.CompareAndSwap(false, true) {
		return // already retrying
	}
	go rm.loop()
}

func (rm *reconnectMonitor) loop() {
	defer rm.running.Store(false)
	rm.emit("⟳ generation unreachable — auto-reconnecting (5× every 1m, then every 5m)")
	fast, slow := reconnectFastEvery, reconnectSlowEvery
	if rm.fastEvery > 0 {
		fast = rm.fastEvery
	}
	if rm.slowEvery > 0 {
		slow = rm.slowEvery
	}
	for attempt := 1; ; attempt++ {
		interval := fast
		if attempt > reconnectFastTries {
			interval = slow
		}
		select {
		case <-rm.ctx.Done():
			return
		case <-time.After(interval):
		}

		ep, ok := rm.activeEndpoint()
		if !ok {
			rm.emit("⟳ reconnect stopped: no active model configured")
			return
		}
		pctx, cancel := context.WithTimeout(rm.ctx, 4*time.Second)
		up := reachable(pctx, ep+"/v1/models")
		cancel()
		if up {
			rm.emit(fmt.Sprintf("✓ generation reconnected: %s — resend your message", ep))
			return
		}
		next := "1m"
		if attempt+1 > reconnectFastTries {
			next = "5m"
		}
		rm.emit(fmt.Sprintf("⟳ reconnect attempt %d failed (%s unreachable) — retrying in %s", attempt, ep, next))
	}
}

// activeEndpoint reads the current active provider's endpoint from config, so
// the monitor follows a /model swap (the active provider may have changed).
func (rm *reconnectMonitor) activeEndpoint() (string, bool) {
	lc, err := loadLaunchConfig(rm.cfgDir)
	if err != nil {
		return "", false
	}
	p := lc.active()
	if p == nil {
		return "", false
	}
	ep := firstNonEmpty(p.Endpoint, providerKinds[p.Kind].DefaultEndpoint)
	if ep == "" {
		return "", false
	}
	return ep, true
}

// emit sends a feed line, dropping nothing under normal load (the System
// channel is buffered) but never blocking past app shutdown.
func (rm *reconnectMonitor) emit(s string) {
	select {
	case rm.feed <- s:
	case <-rm.ctx.Done():
	}
}
