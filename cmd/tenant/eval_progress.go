package main

// eval_progress.go — live progress state for a running quality check, shared
// between the nightly/manual eval job (writer) and the dashboard Quality page
// (reader). Before this, a 3-hour run was invisible between "started" and the
// score line; the operator asked, reasonably, "where's the quality check at?"
// and the honest answer was an elapsed-time guess.

import (
	"sync"
	"time"
)

type evalProgress struct {
	mu        sync.Mutex
	running   bool
	done      int
	total     int
	task      string // last task to finish — "what is it chewing on"
	startedAt time.Time
}

func (p *evalProgress) begin() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	p.done, p.total = 0, 0
	p.task = ""
	p.startedAt = time.Now()
}

func (p *evalProgress) step(done, total int, task string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done, p.total, p.task = done, total, task
}

func (p *evalProgress) finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
}

// view returns render-ready state: running, done, total, last task, minutes
// elapsed.
func (p *evalProgress) view() (bool, int, int, string, int) {
	if p == nil {
		return false, 0, 0, "", 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mins := 0
	if p.running {
		mins = int(time.Since(p.startedAt).Minutes())
	}
	return p.running, p.done, p.total, p.task, mins
}
