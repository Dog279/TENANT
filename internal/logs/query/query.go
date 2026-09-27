// Package query is the log book's filter language (docs/design/logging-v2.md,
// Appendix A). A filter is terms separated by spaces, all of which must hold:
//
//	level>=warning log:discord id:2001-2999 -id:2005 since:24h "reset"
//
// A term is a field condition, field op value[,value…], or full text: a bare
// word or a "quoted phrase", searched in the message. A leading '-' negates a
// term. Values in a list are alternatives. The fields:
//
//	log provider task opcode tag     names: exact, or a glob with * and ? (mcp.*)
//	session approval tool_call actor surface
//	trace activity related boot      whole IDs; boot:current is the reading process's run
//	id pid                           numbers, ranges (2001-2999) and comparisons
//	level                            critical > error > warning > info > verbose,
//	                                 so level>=warning is warning, error and critical
//	channel                          operational or diagnostic
//	since until                      a duration back from now (15m, 2h, 7d, 2w) or a time:
//	                                 RFC 3339, a date, 14:03, today or yesterday (local);
//	                                 until is exclusive
//	data.<key>                       a field of the event's data: text, a glob, or a
//	                                 number, which also compares (data.iter>=3)
//
// The operators are : = != >= <= > <. ':' and '=' mean the same; comparisons
// take one value, on id, pid, level and data only. Inside quotes, \" is a
// quote and \\ a backslash. A word shaped like a field term (name:…) must be
// quoted to be searched.
//
// Parse resolves what the text names (numbers, levels, times), so a Filter
// holds no relative time; String prints it back, and parsing that gives the
// same Filter. internal/logs/store plans a Filter into SQL.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"tenant/internal/logs/event"
)

// Filter is a parsed filter: its terms, all of which must hold.
type Filter struct {
	Terms []Term
}

// Term is one condition.
type Term struct {
	Not    bool    // written with a leading '-'
	Field  string  // log, provider, …, data.<key>; "" for full text
	Op     Op      // "" for full text
	Values []Value // alternatives: any may match; full text has one
}

// Op is a term's comparison.
type Op string

// The operators. Match and Eq mean the same.
const (
	Match Op = ":"
	Eq    Op = "="
	Ne    Op = "!="
	Ge    Op = ">="
	Le    Op = "<="
	Gt    Op = ">"
	Lt    Op = "<"
)

// Value is one alternative of a term, with what Parse resolved it to.
type Value struct {
	Text   string    // as written, without its quotes
	Quoted bool      // written in quotes: never a glob, and for data.<key> text, not a number
	Glob   bool      // a pattern: unquoted, with * or ?, on a name, text or data field
	Lo, Hi int64     // id, pid: the number, or the range Lo-Hi; level: its number (1 critical … 5 verbose); channel: 0 operational, 1 diagnostic
	Time   time.Time // since, until: the instant, UTC
}

// SyntaxError is why a filter doesn't parse, and where.
type SyntaxError struct {
	Pos int    // byte offset in the filter
	Msg string // what was wrong, and what was expected
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("filter: column %d: %s", e.Pos+1, e.Msg) }

// kind is what a field's values are.
type kind uint8

const (
	text    kind = iota // exact values and globs
	ident               // whole IDs
	number              // numbers, ranges and comparisons
	level               // level names, compared by severity
	channel             // operational or diagnostic
	when                // one duration or time
	data                // text, globs, numbers and comparisons
)

var fields = map[string]kind{
	"log": text, "provider": text, "task": text, "opcode": text, "tag": text,
	"session": text, "approval": text, "tool_call": text, "actor": text, "surface": text,
	"trace": ident, "activity": ident, "related": ident, "boot": ident,
	"id": number, "pid": number, "level": level, "channel": channel,
	"since": when, "until": when,
}

const fieldList = "log, provider, id, level, channel, task, opcode, tag, trace, activity, related, session, approval, tool_call, actor, surface, pid, boot, since, until or data.<key>"

// Parse reads a filter, resolving times against now in the local zone.
func Parse(s string) (Filter, error) { return ParseAt(s, time.Now()) }

// ParseAt reads a filter, resolving durations against now and local times
// (14:03, today, a date) in now's location.
func ParseAt(s string, now time.Time) (Filter, error) {
	p := parser{s: s, now: now}
	var f Filter
	for {
		for p.i < len(s) && isSpace(s[p.i]) {
			p.i++
		}
		if p.i == len(s) {
			return f, nil
		}
		t, err := p.term()
		if err != nil {
			return Filter{}, err
		}
		f.Terms = append(f.Terms, t)
	}
}

type parser struct {
	s   string
	i   int
	now time.Time
}

func (p *parser) fail(pos int, format string, args ...any) error {
	return &SyntaxError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) term() (Term, error) {
	var t Term
	if p.s[p.i] == '-' {
		t.Not = true
		p.i++
		if p.i == len(p.s) || isSpace(p.s[p.i]) || p.s[p.i] == '-' {
			return t, p.fail(p.i-1, "expected a field or text after -")
		}
	}
	start := p.i
	if p.s[p.i] == '"' {
		v, err := p.quoted()
		if err != nil {
			return t, err
		}
		if strings.TrimSpace(v.Text) == "" {
			return t, p.fail(start, "expected text inside the quotes")
		}
		if p.i < len(p.s) && !isSpace(p.s[p.i]) {
			return t, p.fail(p.i, "expected a space after the closing quote")
		}
		t.Values = []Value{v}
		return t, nil
	}
	if n := fieldPrefix(p.s[p.i:]); n > 0 {
		return p.condition(t, p.s[start:start+n])
	}
	for p.i < len(p.s) && !isSpace(p.s[p.i]) {
		if p.s[p.i] == '"' {
			return t, p.fail(p.i, "a quote inside a word; quote the whole phrase")
		}
		p.i++
	}
	t.Values = []Value{{Text: p.s[start:p.i]}}
	return t, nil
}

// condition reads the rest of a field term, from its name.
func (p *parser) condition(t Term, name string) (Term, error) {
	start := p.i
	k, ok := fields[name]
	if key, isData := strings.CutPrefix(name, "data."); isData {
		for _, seg := range strings.Split(key, ".") {
			if seg == "" {
				return t, p.fail(start, "expected a key after data., like data.tool or data.req.method")
			}
		}
		k, ok = data, true
	}
	if !ok {
		return t, p.fail(start, "unknown field %q: expected %s; quote text to search for it", name, fieldList)
	}
	t.Field = name
	p.i += len(name)
	opPos := p.i
	for _, op := range []Op{Ne, Ge, Le, Match, Eq, Gt, Lt} {
		if strings.HasPrefix(p.s[p.i:], string(op)) {
			t.Op = op
			p.i += len(op)
			break
		}
	}
	compare := t.Op == Ge || t.Op == Le || t.Op == Gt || t.Op == Lt
	switch {
	case t.Op == "":
		return t, p.fail(opPos, "expected an operator after %s: one of : = != >= <= > <", name)
	case k == when && t.Op != Match && t.Op != Eq:
		return t, p.fail(opPos, "%s takes : or =, not %s", name, t.Op)
	case compare && k != number && k != level && k != data:
		return t, p.fail(opPos, "%s takes : = or !=, not %s; comparisons are for id, pid, level and data.<key>", name, t.Op)
	}
	for {
		pos := p.i
		v, err := p.item()
		if err != nil {
			return t, err
		}
		if err := p.resolve(k, t, &v, pos); err != nil {
			return t, err
		}
		t.Values = append(t.Values, v)
		if p.i == len(p.s) || isSpace(p.s[p.i]) {
			return t, nil
		}
		if p.s[p.i] != ',' {
			return t, p.fail(p.i, "expected , or a space after the value")
		}
		if compare || k == when {
			return t, p.fail(p.i, "%s%s takes one value, not a list", name, t.Op)
		}
		p.i++
	}
}

// item reads one value of a list: quoted, or up to a comma or a space.
func (p *parser) item() (Value, error) {
	if p.i < len(p.s) && p.s[p.i] == '"' {
		return p.quoted()
	}
	start := p.i
	for p.i < len(p.s) && !isSpace(p.s[p.i]) && p.s[p.i] != ',' {
		if p.s[p.i] == '"' {
			return Value{}, p.fail(p.i, "a quote inside a value; quote the whole value")
		}
		p.i++
	}
	if p.i == start {
		return Value{}, p.fail(start, "expected a value")
	}
	return Value{Text: p.s[start:p.i]}, nil
}

// quoted reads a quoted string at p.i. \" is a quote and \\ a backslash; any
// other backslash is itself, so a Windows path needs no doubling.
func (p *parser) quoted() (Value, error) {
	start := p.i
	var b strings.Builder
	for p.i++; p.i < len(p.s); p.i++ {
		switch c := p.s[p.i]; {
		case c == '"':
			p.i++
			return Value{Text: b.String(), Quoted: true}, nil
		case c == '\\' && p.i+1 < len(p.s) && (p.s[p.i+1] == '"' || p.s[p.i+1] == '\\'):
			p.i++
			b.WriteByte(p.s[p.i])
		default:
			b.WriteByte(c)
		}
	}
	return Value{}, p.fail(start, "a quote that is never closed")
}

// resolve checks v against its field and fills in what it names.
func (p *parser) resolve(k kind, t Term, v *Value, pos int) error {
	compare := t.Op == Ge || t.Op == Le || t.Op == Gt || t.Op == Lt
	glob := !v.Quoted && strings.ContainsAny(v.Text, "*?")
	switch k {
	case text:
		v.Glob = glob
	case data:
		if glob && compare {
			return p.fail(pos, "a comparison takes a value, not a pattern")
		}
		v.Glob = glob
	case ident:
		if glob {
			return p.fail(pos, "%s takes a whole ID, not a pattern", t.Field)
		}
	case number:
		lo, hi, isRange := strings.Cut(v.Text, "-")
		if !isRange {
			hi = lo
		}
		max := uint64(1<<63 - 1)
		if t.Field == "id" {
			max = 1<<32 - 1
		}
		l, err1 := strconv.ParseUint(lo, 10, 64)
		h, err2 := strconv.ParseUint(hi, 10, 64)
		switch {
		case err1 != nil || err2 != nil || l > max || h > max:
			return p.fail(pos, "%s takes a number or a range like 2001-2999, not %q", t.Field, v.Text)
		case isRange && compare:
			return p.fail(pos, "a comparison takes one number, not a range")
		case l > h:
			return p.fail(pos, "the range %s runs backwards", v.Text)
		}
		v.Lo, v.Hi = int64(l), int64(h)
	case level:
		l, ok := event.ParseLevel(v.Text)
		if !ok {
			return p.fail(pos, "level takes critical, error, warning, info or verbose, not %q", v.Text)
		}
		v.Lo, v.Hi = int64(l), int64(l)
	case channel:
		switch v.Text {
		case "operational":
		case "diagnostic":
			v.Lo, v.Hi = 1, 1
		default:
			return p.fail(pos, "channel takes operational or diagnostic, not %q", v.Text)
		}
	case when:
		tm, ok := p.instant(v.Text)
		if !ok {
			return p.fail(pos, "%s takes a duration (15m, 2h, 7d) or a time (RFC 3339, 2026-09-27, 14:03, today, yesterday), not %q", t.Field, v.Text)
		}
		v.Time = tm
	}
	return nil
}

// instant is the time s names: a duration back from now, or a time, local
// ones in now's location.
func (p *parser) instant(s string) (time.Time, bool) {
	if d, ok := duration(s); ok {
		return p.now.Add(-d).UTC(), true
	}
	loc := p.now.Location()
	y, m, d := p.now.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, loc)
	switch s {
	case "today":
		return midnight.UTC(), true
	case "yesterday":
		return midnight.AddDate(0, 0, -1).UTC(), true
	}
	for _, layout := range []string{"15:04", "15:04:05"} {
		if c, err := time.ParseInLocation(layout, s, loc); err == nil {
			return time.Date(y, m, d, c.Hour(), c.Minute(), c.Second(), 0, loc).UTC(), true
		}
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// duration reads a non-negative duration: Go's form (90s, 1h30m), or whole
// days (7d) or weeks (2w).
func duration(s string) (time.Duration, bool) {
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			v, err := strconv.ParseUint(n, 10, 32)
			return time.Duration(v) * unit, err == nil && v <= 100_000 // about 270 years
		}
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d >= 0
}

// fieldPrefix is the length of the field name s starts with when s is a
// field term (a name that starts with a letter, then an operator), else 0.
func fieldPrefix(s string) int {
	n := 0
	for n < len(s) && (isLetter(s[n]) || s[n] >= '0' && s[n] <= '9' || s[n] == '_' || s[n] == '.' || s[n] == '-') {
		n++
	}
	if n == 0 || !isLetter(s[0]) || n == len(s) || !strings.ContainsRune(":=!<>", rune(s[n])) {
		return 0
	}
	return n
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isSpace(c byte) bool { return c == ' ' || c >= '\t' && c <= '\r' }

// String prints f in the filter language; Parse reads it back as f.
func (f Filter) String() string {
	var b strings.Builder
	for i, t := range f.Terms {
		if i > 0 {
			b.WriteByte(' ')
		}
		if t.Not {
			b.WriteByte('-')
		}
		if t.Field == "" {
			for j, v := range t.Values {
				if j > 0 {
					b.WriteByte(' ')
				}
				writeValue(&b, v, !v.Quoted && bareWord(v.Text))
			}
			continue
		}
		b.WriteString(t.Field)
		b.WriteString(string(t.Op))
		for j, v := range t.Values {
			if j > 0 {
				b.WriteByte(',')
			}
			writeValue(&b, v, !v.Quoted && v.Text != "" && !strings.ContainsAny(v.Text, " \t\n\v\f\r,\""))
		}
	}
	return b.String()
}

func writeValue(b *strings.Builder, v Value, bare bool) {
	if bare {
		b.WriteString(v.Text)
		return
	}
	b.WriteByte('"')
	for i := 0; i < len(v.Text); i++ {
		if c := v.Text[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v.Text[i])
	}
	b.WriteByte('"')
}

// bareWord reports whether s reads back as one full-text word unquoted.
func bareWord(s string) bool {
	return s != "" && s[0] != '-' && !strings.ContainsAny(s, " \t\n\v\f\r\"") && fieldPrefix(s) == 0
}
