package main

// eval_cadence.go decides WHEN the nightly eval fires (TEN-196). The improve
// scheduler's clocks are in-memory, so plain interval registration fires at
// every launch (zero lastRun ⇒ due at first tick) and then needs the full
// interval of CONTINUOUS uptime to refire — both wrong for a laptop that
// relaunches thirty times on a dev day and sleeps at night. The fix is
// anacron-style semantics: the clock is seeded from trend.jsonl (the durable
// record every run already writes) and due-ness is a predicate, either
// "interval elapsed since the last recorded run" (eval_every) or "the daily
// wall-clock anchor has passed since the last recorded run" (eval_at).

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/improve"
)

// evalSchedule is the LIVE nightly-eval schedule (TUI /eval). The scheduler
// consults it through DueFunc's dynamic predicate, so the TUI can re-tune or
// disarm the cadence with no restart and no scheduler surgery — the predicate
// reads the current schedule on every tick. A nil inner due means off
// (predicate never fires); forceOnce makes the next unpaused tick fire
// regardless (/eval now), riding the scheduler's own goroutine so the run
// lands in the feed + trend log like any scheduled one. Degraded-mode
// suppression is upstream (Scheduler.Paused), so a forced run still never
// executes against the echo fallback.
type evalSchedule struct {
	mu        sync.Mutex
	due       improve.DueFunc
	desc      string
	forceOnce bool
}

func newEvalSchedule(due improve.DueFunc, desc string) *evalSchedule {
	return &evalSchedule{due: due, desc: desc}
}

// set swaps the schedule in place (nil due ⇒ off).
func (s *evalSchedule) set(due improve.DueFunc, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.due, s.desc = due, desc
}

// Desc returns the human description of the current schedule ("off",
// "every 24h0m0s", "daily at 03:15").
func (s *evalSchedule) Desc() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desc
}

// ForceOnce queues exactly one immediate fire at the next scheduler tick.
func (s *evalSchedule) ForceOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceOnce = true
}

// Pending reports whether a queued one-shot fire (/eval now) has not yet been
// consumed by a tick — surfaced by /eval status so a queued run is visible.
func (s *evalSchedule) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forceOnce
}

// DueFunc returns the dynamic predicate registered with the scheduler.
func (s *evalSchedule) DueFunc() improve.DueFunc {
	return func(lastRun, now time.Time) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.forceOnce {
			s.forceOnce = false // consumed exactly once
			return true
		}
		if s.due == nil {
			return false
		}
		return s.due(lastRun, now)
	}
}

// parseEvalAt parses a daily wall-clock anchor "HH:MM" (24-hour, operator-
// local). Returns ok=false for anything else — the caller treats that as
// malformed and falls back, never guesses.
func parseEvalAt(s string) (hh, mm int, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	if herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// mostRecentAnchor returns the latest occurrence of hh:mm (in now's location)
// that is not after now — today's anchor if it has already passed, else
// yesterday's. AddDate handles month/year boundaries and DST transitions.
func mostRecentAnchor(now time.Time, hh, mm int) time.Time {
	a := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if a.After(now) {
		a = a.AddDate(0, 0, -1)
	}
	return a
}

// evalEveryDue is interval semantics with a restart-surviving clock: due when
// the (trend-seeded) last run is at least d old. A zero lastRun — no trend
// history yet — fires at the first tick, which is the right bootstrap: the
// very first enabled launch produces the first data point.
func evalEveryDue(d time.Duration) improve.DueFunc {
	return func(lastRun, now time.Time) bool {
		return lastRun.IsZero() || now.Sub(lastRun) >= d
	}
}

// evalAtDue is anacron-style daily-anchor semantics: due when the last run
// predates the most recent hh:mm occurrence. A box asleep at the anchor
// catches up on the first tick after wake; a relaunch after today's run does
// not re-fire until tomorrow's anchor; a day is never double-fired.
func evalAtDue(hh, mm int) improve.DueFunc {
	return func(lastRun, now time.Time) bool {
		return lastRun.Before(mostRecentAnchor(now, hh, mm))
	}
}

// resolveEvalDue picks the nightly-eval schedule (TEN-196 supersedes the
// duration-only resolveEvalCadence for the eval job; soul-nudge still uses
// the latter). Precedence: an explicitly-set --eval-every flag wins (direct
// operator intent), else a valid improve.eval_at daily anchor, else the
// improve.eval_every interval, else off (nil DueFunc). A malformed eval_at
// warns and falls through to eval_every — fail closed to the weaker
// schedule, never to a surprise default. tickHint is the interval when one
// exists (feeds the scheduler-tick minimum; 0 for anchor mode, which the
// ≤1m tick cap already serves). log may be nil.
func resolveEvalDue(flagSet bool, flagVal time.Duration, cfgEvery, cfgAt string, log *slog.Logger) (due improve.DueFunc, tickHint time.Duration, desc string) {
	if flagSet {
		if flagVal <= 0 {
			return nil, 0, "off (flag)"
		}
		return evalEveryDue(flagVal), flagVal, "every " + flagVal.String() + " (flag)"
	}
	if at := strings.TrimSpace(cfgAt); at != "" {
		if hh, mm, ok := parseEvalAt(at); ok {
			return evalAtDue(hh, mm), 0, fmt.Sprintf("daily at %02d:%02d", hh, mm)
		}
		if log != nil {
			log.Warn("ignoring malformed improve.eval_at; falling back to improve.eval_every", "value", at)
		}
	}
	if d := resolveEvalCadence(false, 0, cfgEvery, log); d > 0 {
		return evalEveryDue(d), d, "every " + d.String()
	}
	return nil, 0, "off"
}
