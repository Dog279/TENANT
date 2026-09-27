// Package logs is Tenant's unified log book: every subsystem — the agent's
// turns, sub-agents, approvals, the Discord and iMessage relays, cron, eval,
// the model router, plain slog records — writes through ONE Hub that files each
// line under a per-source folder of plain-text daily files
// (<data>/logs/<source>/YYYY-MM-DD.log) and keeps a retained, cursor-indexed
// ring the dashboard Activity feed replays from. A source is a folder is a tab:
// naming a new source anywhere (a Go logger, an HTTP POST, the `tenant log`
// CLI, or a script that just appends to a new folder) creates the folder and
// the dashboard tab without a rebuild.
//
// The on-disk line format is designed for humans first (readable in `tail -f`,
// grep-able, one entry per line) and is round-trippable so lines written by
// other processes — or by an earlier run of this one — can be read back into
// the feed:
//
//	2026-09-22 14:03:11.123 INFO  [turns] tool call web_search	ev=tool_call iter=2 args="{\"q\":\"…\"}"
//	<date> <time.ms>        <LVL> [<source>] <message>[<TAB><key=value …>]
package logs

import (
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"tenant/internal/logs/event"
)

// Field is one key=value pair carried by an Entry. Values are plain strings;
// the writer quotes them on the way to disk when they contain spaces, quotes or
// control characters.
type Field struct {
	Key   string
	Value string
}

// Entry is one log line as the hub retains and writes it.
type Entry struct {
	Seq    uint64 // hub-assigned monotonic sequence (0 until logged)
	At     time.Time
	Source string     // the folder/tab this line belongs to ("" ⇒ DefaultSource)
	Level  slog.Level // DEBUG/INFO/WARN/ERROR (custom levels are bucketed on write)
	Msg    string
	Fields []Field
}

// DefaultSource is where records land when nothing names a source.
const DefaultSource = "system"

// LevelCritical is the level above ERROR (Event Viewer's Critical): a crash, a
// lost store, a broken audit chain. v1 files write it as ERROR (LevelName
// buckets it) with a severity=critical field.
const LevelCritical = slog.LevelError + 4

// Field returns the value of the first field named key ("" when absent).
func (e Entry) Field(key string) string {
	for _, f := range e.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// Kind is the agent event kind this entry was rendered from (the `ev` field),
// or "" for a plain log record. The dashboard uses it to pick the row style.
func (e Entry) Kind() string { return e.Field("ev") }

// IsErr reports whether the entry is error-level.
func (e Entry) IsErr() bool { return e.Level >= slog.LevelError }

// With returns a copy of the entry with the given key/value appended.
func (e Entry) With(key, value string) Entry {
	fields := make([]Field, 0, len(e.Fields)+1)
	fields = append(fields, e.Fields...)
	fields = append(fields, Field{Key: key, Value: value})
	e.Fields = fields
	return e
}

// timeLayout is the on-disk timestamp: local wall clock with milliseconds —
// what a person auditing a day's file wants to read, and unambiguous enough to
// parse back (ParseLine reads it in the local zone).
const timeLayout = "2006-01-02 15:04:05.000"

// LevelName is the fixed-width level column: custom slog levels are bucketed
// into the four names so files stay uniform.
func LevelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

// ParseLevel is LevelName's inverse (case-insensitive; unknown ⇒ INFO, false).
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug, true
	case "INFO", "":
		return slog.LevelInfo, true
	case "WARN", "WARNING":
		return slog.LevelWarn, true
	case "ERROR", "ERR", "FATAL":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// Line renders the entry as one on-disk line (no trailing newline). Message
// and values are flattened to a single line: newlines and tabs in the message
// become spaces and its other control characters are escaped; values that
// need it are strconv-quoted (which escapes \n, \t, quotes and control
// characters), so one entry is always exactly one line, `grep` stays exact,
// and nothing in a line can drive the terminal of a `tail -f`.
func (e Entry) Line() string {
	var b strings.Builder
	b.Grow(96 + len(e.Msg))
	b.WriteString(e.At.Format(timeLayout))
	b.WriteByte(' ')
	lvl := LevelName(e.Level)
	b.WriteString(lvl)
	for i := len(lvl); i < 5; i++ {
		b.WriteByte(' ')
	}
	b.WriteString(" [")
	src := e.Source
	if src == "" {
		src = DefaultSource
	}
	b.WriteString(src)
	b.WriteString("] ")
	b.WriteString(flattenMsg(e.Msg))
	if len(e.Fields) > 0 {
		b.WriteByte('\t')
		for i, f := range e.Fields {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(f.Key)
			b.WriteByte('=')
			b.WriteString(quoteValue(f.Value))
		}
	}
	return b.String()
}

// flattenMsg collapses a message onto one line. Tabs are the message/field
// separator on disk, so they must not survive inside the message. Every
// other control character (ESC, DEL, the C1 set) and any byte that isn't
// UTF-8 is escaped as Go would quote it (\x1b, \u009b), so v1 parsers still
// read the line and a terminal showing it runs no escape sequence.
func flattenMsg(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == utf8.RuneError }) < 0 {
		return s // already clean
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case strings.HasPrefix(s[i:], "\r\n"):
			b.WriteByte(' ')
			n = 2
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || r == utf8.RuneError && n == 1:
			q := strconv.Quote(s[i : i+n])
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// quoteValue writes a field value bare when it is boring, quoted otherwise.
func quoteValue(v string) string {
	if v == "" {
		return `""`
	}
	for _, r := range v {
		if r <= ' ' || r == '"' || r == '\\' || r == '=' || unicode.IsControl(r) || r == utf8.RuneError {
			return strconv.Quote(v)
		}
	}
	return v
}

// ParseLine reads one on-disk line back into an Entry (Seq is 0). ok is false
// when the line does not follow the format — callers keep such lines as raw
// messages so foreign files still show up rather than vanish.
func ParseLine(line string) (Entry, bool) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) < len(timeLayout)+8 {
		return Entry{}, false
	}
	at, err := time.ParseInLocation(timeLayout, line[:len(timeLayout)], time.Local)
	if err != nil {
		return Entry{}, false
	}
	rest := line[len(timeLayout):]
	if rest == "" || rest[0] != ' ' {
		return Entry{}, false
	}
	rest = strings.TrimLeft(rest, " ")
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return Entry{}, false
	}
	lvl, ok := ParseLevel(rest[:sp])
	if !ok {
		return Entry{}, false
	}
	rest = strings.TrimLeft(rest[sp:], " ")
	if !strings.HasPrefix(rest, "[") {
		return Entry{}, false
	}
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return Entry{}, false
	}
	src := rest[1:end]
	rest = strings.TrimPrefix(rest[end+1:], " ")
	e := Entry{At: at, Level: lvl, Source: src}
	if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
		e.Msg = rest[:tab]
		e.Fields = parseFields(rest[tab+1:])
	} else {
		e.Msg = rest
	}
	return e, true
}

// parseFields tokenizes `k=v k2="quoted v"` (best-effort: a malformed tail is
// kept as a single raw field so nothing is silently dropped).
func parseFields(s string) []Field {
	var out []Field
	for s != "" {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 || strings.ContainsAny(s[:eq], " \t\"") {
			out = append(out, Field{Key: "raw", Value: s})
			break
		}
		key := s[:eq]
		s = s[eq+1:]
		if strings.HasPrefix(s, `"`) {
			// Quoted: find the closing quote honoring backslash escapes.
			i := 1
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '"' {
					break
				}
				i++
			}
			if i >= len(s) {
				out = append(out, Field{Key: key, Value: s})
				break
			}
			val, err := strconv.Unquote(s[:i+1])
			if err != nil {
				val = s[1:i]
			}
			out = append(out, Field{Key: key, Value: val})
			s = s[i+1:]
			continue
		}
		sp := strings.IndexByte(s, ' ')
		if sp < 0 {
			out = append(out, Field{Key: key, Value: s})
			break
		}
		out = append(out, Field{Key: key, Value: s[:sp]})
		s = s[sp:]
	}
	return out
}

// ValidSource reports whether name is an acceptable source name as-is. It
// is a path segment we create under the log root, so it can never be "..",
// carry a separator, start with a dot, end with one, or be a Windows device
// name: it is a log name (event.ValidName).
func ValidSource(name string) bool { return event.ValidName(name) }

// NormalizeSource coerces a free-form label into a valid source name: lowercase,
// spaces and other punctuation → '-', trimmed to 40 chars. Returns "" when
// nothing usable remains (callers then fall back to DefaultSource or reject).
func NormalizeSource(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-._")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-._")
	}
	if !ValidSource(s) {
		return ""
	}
	return s
}

// capValue truncates v to at most max bytes (on a rune boundary), appending a
// marker with the number of bytes dropped so a reader knows the value was cut.
func capValue(v string, max int) string {
	if max <= 0 || len(v) <= max {
		return v
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut] + "…(+" + strconv.Itoa(len(v)-cut) + " bytes)"
}
