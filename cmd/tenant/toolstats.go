package main

// toolstats.go — the per-tool dispatch counter (Quality v2, Phase 1).
//
// Every tool call in the system funnels through toolMux.Dispatch (agent turns,
// cron jobs via their restricted views, the MCP bridge via mcpserver), so ONE
// counter site yields honest usage and error rates for every surface. Before
// this, no per-tool counter existed anywhere: usage questions were answered by
// grepping two months of archive JSONL, and cron/bridge traffic wasn't even
// there (cron agents are memory-isolated; the bridge dispatches outside the
// agent loop).
//
// Each PROCESS writes its own file — toolstats-<role>-<pid>.json — because
// same-role processes can genuinely coexist (stdio-mode bridges spawn one per
// MCP client) and snapshot rewrites of a shared file are last-writer-wins:
// review of the first version showed two bridges silently erasing each
// other's counts. Readers sum across files. On startup a process absorbs
// same-role files whose heartbeat has gone stale (a live process rewrites its
// file every minute, dirty or not, precisely so stale means dead) — summed
// once into the newcomer and deleted, so counts survive restarts without
// double-counting.
//
// The eval harness deliberately never enables stats: 48 synthetic tasks a
// night would drown the real usage signal.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const toolStatsSchemaVersion = 1

// staleAfter is how long without a heartbeat before another process may
// absorb a same-role file. Live processes rewrite every persistInterval.
const (
	persistInterval = time.Minute
	staleAfter      = 10 * time.Minute
)

// toolStat is one tool's cumulative dispatch record.
type toolStat struct {
	Calls        uint64 `json:"calls"`
	Errors       uint64 `json:"errors,omitempty"`
	ConfigErrors uint64 `json:"config_errors,omitempty"`
	// DisabledCalls counts attempts on a disabled tool — "the model wants
	// this while it's off" is a signal the operator should see.
	DisabledCalls uint64 `json:"disabled_calls,omitempty"`
	// Canceled counts calls cut short by context cancellation (operator ESC,
	// sub-agent teardown). Kept out of Errors: a long web call that gets
	// interrupted often is not an unhealthy tool.
	Canceled   uint64 `json:"canceled,omitempty"`
	LastCall   int64  `json:"last_call,omitempty"` // unix seconds
	LastOK     int64  `json:"last_ok,omitempty"`
	LastErr    int64  `json:"last_err,omitempty"`
	LastErrMsg string `json:"last_err_msg,omitempty"`
}

func (a *toolStat) absorb(b *toolStat) {
	a.Calls += b.Calls
	a.Errors += b.Errors
	a.ConfigErrors += b.ConfigErrors
	a.DisabledCalls += b.DisabledCalls
	a.Canceled += b.Canceled
	if b.LastCall > a.LastCall {
		a.LastCall = b.LastCall
	}
	if b.LastOK > a.LastOK {
		a.LastOK = b.LastOK
	}
	if b.LastErr > a.LastErr {
		a.LastErr = b.LastErr
		if b.LastErrMsg != "" {
			a.LastErrMsg = b.LastErrMsg
		}
	}
}

type toolStatsFile struct {
	Schema  int                  `json:"schema"`
	Role    string               `json:"role"`
	Updated string               `json:"updated"`
	Tools   map[string]*toolStat `json:"tools"`
}

// toolStats is the live counter set. Safe for concurrent Dispatch callers.
type toolStats struct {
	mu    sync.Mutex
	tools map[string]*toolStat

	// flushMu serializes file writes: the persist ticker and the stop func can
	// otherwise write the same tmp concurrently, and a torn tmp that wins its
	// rename resets all history at next load.
	flushMu sync.Mutex

	role string
	path string
	now  func() time.Time
}

func newToolStats(dataDir, role string) *toolStats {
	s := &toolStats{
		tools: map[string]*toolStat{},
		role:  role,
		path:  filepath.Join(dataDir, fmt.Sprintf("toolstats-%s-%d.json", role, os.Getpid())),
		now:   time.Now,
	}
	s.absorbAbandoned(dataDir)
	return s
}

// absorbAbandoned folds dead same-role files (and the legacy un-pidded file
// from the first schema) into this process's counts, then deletes them —
// summed exactly once, so restarts keep history without double-counting.
func (s *toolStats) absorbAbandoned(dataDir string) {
	matches, _ := filepath.Glob(filepath.Join(dataDir, "toolstats-"+s.role+"*.json"))
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var f toolStatsFile
		if json.Unmarshal(data, &f) != nil || f.Tools == nil {
			// Unparseable → abandoned garbage either way; remove it so it
			// cannot shadow real history forever.
			_ = os.Remove(m)
			continue
		}
		// Own-path files are always absorbable: no LIVE process can share this
		// PID, so whatever sits at our name is a dead predecessor's counts —
		// including a fast restart that recycled the PID inside staleAfter.
		legacy := m == filepath.Join(dataDir, "toolstats-"+s.role+".json")
		if !legacy && m != s.path {
			if ts, terr := time.Parse(time.RFC3339, f.Updated); terr == nil && s.now().Sub(ts) < staleAfter {
				continue // fresh heartbeat — a live sibling owns this file
			}
		}
		for name, st := range f.Tools {
			mine, ok := s.tools[name]
			if !ok {
				mine = &toolStat{}
				s.tools[name] = mine
			}
			mine.absorb(st)
		}
		_ = os.Remove(m)
	}
}

// Outcome classes for record.
const (
	statOK       = "ok"
	statErr      = "error"
	statConfig   = "config_error"
	statDisabled = "disabled"
	statCanceled = "canceled"
)

// classifyDispatch maps a dispatch result to an outcome class. Config errors
// are recognized by the mux's own constant refusal shapes — a tool that
// answers "not configured" is broken in a way the operator fixes once, not a
// runtime flake, and the health pillar treats the two very differently.
// Context cancellation is its own class: an interrupted call says nothing
// about the tool's health.
func classifyDispatch(out string, isErr bool, err error) string {
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "context canceled") || strings.Contains(msg, "context deadline exceeded") {
			return statCanceled
		}
	}
	if err == nil && !isErr {
		return statOK
	}
	if strings.Contains(out, "not configured") || strings.Contains(out, "is disabled (/enable") {
		return statConfig
	}
	return statErr
}

func (s *toolStats) record(name, outcome string) {
	if s == nil {
		return
	}
	now := s.now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tools[name]
	if !ok {
		st = &toolStat{}
		s.tools[name] = st
	}
	if outcome == statDisabled {
		st.DisabledCalls++
		return
	}
	st.Calls++
	st.LastCall = now
	switch outcome {
	case statOK:
		st.LastOK = now
	case statCanceled:
		st.Canceled++
	case statConfig:
		st.ConfigErrors++
		st.LastErr = now
	default:
		st.Errors++
		st.LastErr = now
	}
}

// noteError attaches the last error text (truncated) — split from record so
// the mutex isn't held while callers format.
func (s *toolStats) noteError(name, msg string) {
	if s == nil {
		return
	}
	const cap = 160
	if len(msg) > cap {
		msg = msg[:cap]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.tools[name]; ok {
		st.LastErrMsg = msg
	}
}

// flush writes the current snapshot unconditionally — the write doubles as
// the liveness heartbeat absorbAbandoned relies on, and always-write means a
// failed write never strands unsaved state behind a cleared dirty flag.
func (s *toolStats) flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	f := toolStatsFile{
		Schema:  toolStatsSchemaVersion,
		Role:    s.role,
		Updated: s.now().UTC().Format(time.RFC3339),
		Tools:   make(map[string]*toolStat, len(s.tools)),
	}
	for k, v := range s.tools {
		cp := *v
		f.Tools[k] = &cp
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	// Write-temp-then-rename so a crash mid-write never truncates history.
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// startPersist flushes on a ticker until the returned stop func runs (which
// flushes one final time).
func (s *toolStats) startPersist(every time.Duration) func() {
	if s == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.flush()
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			s.flush()
		})
	}
}
