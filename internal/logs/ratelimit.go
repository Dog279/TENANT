package logs

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"tenant/internal/logs/event"
)

// Rate limits (docs/design/logging-v2.md §12.1): a token bucket per provider
// and event ID, refilled at the rate up to the burst. An uncatalogued event
// (ID 0) is bucketed by its provider and its message's template, so one
// chatty line can't silence the rest of its provider. The Security log is
// never limited: enqueue commits it before it gets here. What a bucket
// refuses is counted, and the housekeeper reports it once a minute.
const (
	defaultRate  = 100
	defaultBurst = 500
	templateMax  = 256 // the bytes of a message its template is made from
)

type limitKey struct {
	provider string
	id       uint32
	tmpl     string // an uncatalogued event's template
}

type bucket struct {
	tokens float64
	last   time.Time
}

// suppression counts one key's refused events, and the log they were for.
type suppression struct {
	log string
	n   uint64
}

// limiter is safe for concurrent use; its zero value has the default limit.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens a second: 0 ⇒ the default, <0 ⇒ no limit
	burst   float64
	buckets map[limitKey]*bucket
	counts  map[limitKey]*suppression // since the last take
}

// set changes the limit, live: 0 keeps the default, <0 lifts it.
func (l *limiter) set(perSecond float64, burst int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate, l.burst = perSecond, float64(burst)
}

// limits is the rate and burst in force (l.mu held).
func (l *limiter) limits() (rate, burst float64) {
	rate, burst = l.rate, l.burst
	if rate == 0 {
		rate = defaultRate
	}
	if burst <= 0 {
		burst = defaultBurst
	}
	return rate, burst
}

// allow takes a token from e's bucket, or counts e suppressed and says no.
func (l *limiter) allow(e *event.Event) bool {
	now := time.Now()
	k := limitKey{provider: e.Provider, id: e.ID}
	if e.ID == 0 {
		k.tmpl = template(e.Msg)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rate, burst := l.limits()
	if rate < 0 {
		return true
	}
	b := l.buckets[k]
	if b == nil {
		if l.buckets == nil {
			l.buckets = map[limitKey]*bucket{}
		}
		b = &bucket{tokens: burst, last: now}
		l.buckets[k] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	s := l.counts[k]
	if s == nil {
		if l.counts == nil {
			l.counts = map[limitKey]*suppression{}
		}
		s = &suppression{log: e.Log}
		l.counts[k] = s
	}
	s.n++
	return false
}

// take returns what was suppressed since the last take, and forgets the
// buckets that have filled up again, which are as good as new, so the map
// holds only the keys busy lately.
func (l *limiter) take() map[limitKey]*suppression {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.counts
	l.counts = nil
	rate, burst := l.limits()
	now := time.Now()
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*rate >= burst {
			delete(l.buckets, k)
		}
	}
	return out
}

// quoted matches a quoted value: "…" or `…`, or '…' where the quote opens a
// word, so an apostrophe (don't) is left alone.
var quoted = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`|(^|[\\s=:(\\[])'[^']*'")

// template is msg with its quoted values and every word holding a digit
// taken out: the lines one call site writes share it.
func template(msg string) string {
	msg = quoted.ReplaceAllString(msg[:min(len(msg), templateMax)], "$1")
	words := strings.Fields(msg)
	kept := words[:0]
	for _, w := range words {
		if !strings.ContainsAny(w, "0123456789") {
			kept = append(kept, w)
		}
	}
	return strings.Join(kept, " ")
}
