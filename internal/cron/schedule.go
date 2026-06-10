// Package cron is a self-contained, pure-Go recurring-job engine: a crontab/
// interval schedule parser plus a small scheduler that runs an injected Runner
// when jobs come due. It has NO external dependencies and NO build tags — it
// runs and is tested on every OS.
//
// It is deliberately separate from internal/improve (whose CronStore/Scheduler
// are interval-only and SQLite-backed): cron jobs here are operator config
// (persisted in config.json alongside the Relay/IMessage blocks), and they
// support real crontab expressions, not just fixed durations.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// minEvery is the floor for @every interval schedules. Sub-minute intervals
// would fire on every scheduler tick and burn an agent turn each time, so they
// are rejected at parse time (blast-radius / token-burn guard).
const minEvery = time.Minute

// yearSearchCap bounds Next()'s forward search so an impossible spec (e.g.
// "0 0 31 2 *" — Feb 31) terminates and reports "never" instead of looping.
const yearSearchCap = 5

// Schedule is a parsed recurring schedule: either a fixed interval (@every) or
// a 5-field crontab expression. The zero value is not usable — build one with
// Parse. Schedules are immutable after parsing and safe for concurrent reads.
type Schedule struct {
	raw      string
	interval time.Duration // >0 for @every schedules; 0 for cron schedules

	// cron bitsets (bit n set => value n matches). Unused for interval schedules.
	min, hour, dom, month, dow uint64
	// domRestricted/dowRestricted record whether the source field was something
	// other than "*". They drive the classic Vixie-cron OR-rule: when BOTH the
	// day-of-month and day-of-week fields are restricted, a day matches if
	// EITHER matches; otherwise both must match.
	domRestricted, dowRestricted bool
}

// String returns the canonical spec text the schedule was parsed from.
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	return s.raw
}

// IsInterval reports whether this is an @every (fixed-duration) schedule.
func (s *Schedule) IsInterval() bool { return s != nil && s.interval > 0 }

// Parse parses a schedule spec. Accepted forms:
//
//	@every <dur>   fixed interval, e.g. "@every 30m" (minimum 1m)
//	@hourly        => 0 * * * *
//	@daily/@midnight => 0 0 * * *
//	@weekly        => 0 0 * * 0
//	@monthly       => 0 0 1 * *
//	@yearly/@annually => 0 0 1 1 *
//	m h dom mon dow standard 5-field crontab (numeric fields, *, ranges,
//	                lists, and steps). dow 0 or 7 = Sunday.
//
// Out-of-range values, inverted ranges, zero/negative steps, the wrong field
// count, and sub-minute / non-positive intervals are rejected with an error
// (never silently clamped).
func Parse(spec string) (*Schedule, error) {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return nil, fmt.Errorf("cron: empty schedule")
	}

	if strings.HasPrefix(raw, "@") {
		return parseMacro(raw)
	}

	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields (m h dom mon dow) or an @macro, got %d in %q", len(fields), raw)
	}
	return parseCronFields(raw, fields)
}

func parseMacro(raw string) (*Schedule, error) {
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "@every") {
		durStr := strings.TrimSpace(raw[len("@every"):])
		if durStr == "" {
			return nil, fmt.Errorf("cron: @every needs a duration, e.g. @every 30m")
		}
		d, err := time.ParseDuration(durStr)
		if err != nil {
			return nil, fmt.Errorf("cron: bad @every duration %q: %w", durStr, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("cron: @every duration must be positive, got %v", d)
		}
		if d < minEvery {
			return nil, fmt.Errorf("cron: @every duration must be at least %v, got %v", minEvery, d)
		}
		return &Schedule{raw: raw, interval: d}, nil
	}

	var expanded string
	switch lower {
	case "@hourly":
		expanded = "0 * * * *"
	case "@daily", "@midnight":
		expanded = "0 0 * * *"
	case "@weekly":
		expanded = "0 0 * * 0"
	case "@monthly":
		expanded = "0 0 1 * *"
	case "@yearly", "@annually":
		expanded = "0 0 1 1 *"
	default:
		return nil, fmt.Errorf("cron: unknown macro %q", raw)
	}
	s, err := parseCronFields(raw, strings.Fields(expanded))
	if err != nil {
		return nil, err
	}
	return s, nil
}

func parseCronFields(raw string, f []string) (*Schedule, error) {
	min, _, err := parseField(f[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("cron: minute field: %w", err)
	}
	hour, _, err := parseField(f[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("cron: hour field: %w", err)
	}
	dom, domR, err := parseField(f[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("cron: day-of-month field: %w", err)
	}
	month, _, err := parseField(f[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("cron: month field: %w", err)
	}
	dow, dowR, err := parseField(f[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("cron: day-of-week field: %w", err)
	}
	// Normalize dow: 7 == Sunday == 0.
	if dow&(1<<7) != 0 {
		dow |= 1 << 0
		dow &^= 1 << 7
	}
	return &Schedule{
		raw: raw, min: min, hour: hour, dom: dom, month: month, dow: dow,
		domRestricted: domR, dowRestricted: dowR,
	}, nil
}

// parseField parses one crontab field into a bitset over [lo,hi]. restricted is
// true when the field text is anything other than "*" (used for the dom/dow
// OR-rule). Supports "*", single values, "a-b" ranges, comma lists, and "*/n"
// / "a-b/n" / "a/n" steps.
func parseField(expr string, lo, hi int) (set uint64, restricted bool, err error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return 0, false, fmt.Errorf("empty field")
	}
	restricted = expr != "*"
	for _, part := range strings.Split(expr, ",") {
		bits, perr := parsePart(strings.TrimSpace(part), lo, hi)
		if perr != nil {
			return 0, false, perr
		}
		set |= bits
	}
	if set == 0 {
		return 0, false, fmt.Errorf("no values in %q", expr)
	}
	return set, restricted, nil
}

func parsePart(part string, lo, hi int) (uint64, error) {
	if part == "" {
		return 0, fmt.Errorf("empty list element")
	}
	step := 1
	rangePart := part
	if i := strings.IndexByte(part, '/'); i >= 0 {
		rangePart = part[:i]
		stepStr := part[i+1:]
		s, err := strconv.Atoi(stepStr)
		if err != nil {
			return 0, fmt.Errorf("bad step %q", stepStr)
		}
		if s < 1 {
			return 0, fmt.Errorf("step must be >= 1, got %d", s)
		}
		step = s
	}

	var start, end int
	switch {
	case rangePart == "*":
		start, end = lo, hi
	case strings.IndexByte(rangePart, '-') >= 0:
		i := strings.IndexByte(rangePart, '-')
		a, err := strconv.Atoi(strings.TrimSpace(rangePart[:i]))
		if err != nil {
			return 0, fmt.Errorf("bad range start %q", rangePart[:i])
		}
		b, err := strconv.Atoi(strings.TrimSpace(rangePart[i+1:]))
		if err != nil {
			return 0, fmt.Errorf("bad range end %q", rangePart[i+1:])
		}
		if a > b {
			return 0, fmt.Errorf("inverted range %d-%d", a, b)
		}
		start, end = a, b
	default:
		v, err := strconv.Atoi(rangePart)
		if err != nil {
			return 0, fmt.Errorf("bad value %q", rangePart)
		}
		start = v
		// "a/n" (single value with a step) is an open-ended range a..hi.
		if step > 1 {
			end = hi
		} else {
			end = v
		}
	}

	if start < lo || end > hi {
		return 0, fmt.Errorf("value out of range [%d,%d]: %d-%d", lo, hi, start, end)
	}

	var bits uint64
	for v := start; v <= end; v += step {
		bits |= 1 << uint(v)
	}
	return bits, nil
}

// Next returns the first instant strictly after `after` at which the schedule
// fires, in `after`'s location. For interval schedules that is after+interval.
// For cron schedules the search works at whole-minute granularity and is capped
// at yearSearchCap years ahead; an impossible spec returns the zero Time, which
// callers must treat as "never due".
func (s *Schedule) Next(after time.Time) time.Time {
	if s == nil {
		return time.Time{}
	}
	if s.interval > 0 {
		return after.Add(s.interval)
	}

	loc := after.Location()
	// Start at the next whole minute strictly after `after`.
	t := after.Truncate(time.Minute).Add(time.Minute)
	capYear := after.Year() + yearSearchCap

	for {
		if t.Year() > capYear {
			return time.Time{}
		}
		if !bitSet(s.month, int(t.Month())) {
			// Jump to the first day of the next month at 00:00.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !bitSet(s.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !bitSet(s.min, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
}

// dayMatches applies the Vixie-cron OR-rule for day-of-month vs day-of-week.
func (s *Schedule) dayMatches(t time.Time) bool {
	domMatch := bitSet(s.dom, t.Day())
	dowMatch := bitSet(s.dow, int(t.Weekday())) // time.Sunday == 0
	if s.domRestricted && s.dowRestricted {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

func bitSet(set uint64, n int) bool {
	if n < 0 || n > 63 {
		return false
	}
	return set&(1<<uint(n)) != 0
}
