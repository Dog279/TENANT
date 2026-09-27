package query

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// now is 2026-09-27 15:30:00 in a zone two hours east of UTC.
var (
	zone = time.FixedZone("CEST", 2*3600)
	now  = time.Date(2026, 9, 27, 15, 30, 0, 0, zone)
)

func parse(t *testing.T, s string) Filter {
	t.Helper()
	f, err := ParseAt(s, now)
	if err != nil {
		t.Fatalf("ParseAt(%q): %v", s, err)
	}
	return f
}

// The ticket's example, term by term.
func TestParseExample(t *testing.T) {
	f := parse(t, `level>=warning log:discord id:2001-2999 -id:2005 since:24h "reset"`)
	want := []Term{
		{Field: "level", Op: Ge, Values: []Value{{Text: "warning", Lo: 3, Hi: 3}}},
		{Field: "log", Op: Match, Values: []Value{{Text: "discord"}}},
		{Field: "id", Op: Match, Values: []Value{{Text: "2001-2999", Lo: 2001, Hi: 2999}}},
		{Not: true, Field: "id", Op: Match, Values: []Value{{Text: "2005", Lo: 2005, Hi: 2005}}},
		{Field: "since", Op: Match, Values: []Value{{Text: "24h", Time: now.Add(-24 * time.Hour).UTC()}}},
		{Values: []Value{{Text: "reset", Quoted: true}}},
	}
	if !reflect.DeepEqual(f.Terms, want) {
		t.Errorf("got  %+v\nwant %+v", f.Terms, want)
	}
}

func TestParseValues(t *testing.T) {
	day := func(d, h, m int) time.Time { return time.Date(2026, 9, d, h, m, 0, 0, zone).UTC() }
	for _, c := range []struct {
		in   string
		want Value
	}{
		{"provider:mcp.*", Value{Text: "mcp.*", Glob: true}},
		{`provider:"mcp.*"`, Value{Text: "mcp.*", Quoted: true}},
		{"tag:audit.fail?re", Value{Text: "audit.fail?re", Glob: true}},
		{"session:discord:general", Value{Text: "discord:general"}},
		{"pid:4412", Value{Text: "4412", Lo: 4412, Hi: 4412}},
		{"id:4294967295", Value{Text: "4294967295", Lo: 1<<32 - 1, Hi: 1<<32 - 1}},
		{"level:ERR", Value{Text: "ERR", Lo: 2, Hi: 2}},
		{"channel:diagnostic", Value{Text: "diagnostic", Lo: 1, Hi: 1}},
		{"boot:current", Value{Text: "current"}},
		{"since:15m", Value{Text: "15m", Time: now.Add(-15 * time.Minute).UTC()}},
		{"since:1h30m", Value{Text: "1h30m", Time: now.Add(-90 * time.Minute).UTC()}},
		{"since:7d", Value{Text: "7d", Time: now.AddDate(0, 0, -7).UTC()}},
		{"since:2w", Value{Text: "2w", Time: now.AddDate(0, 0, -14).UTC()}},
		{"since:14:03", Value{Text: "14:03", Time: day(27, 14, 3)}},
		{"until:09:05:30", Value{Text: "09:05:30", Time: day(27, 9, 5).Add(30 * time.Second)}},
		{"since:today", Value{Text: "today", Time: day(27, 0, 0)}},
		{"since:yesterday", Value{Text: "yesterday", Time: day(26, 0, 0)}},
		{"since:2026-09-01", Value{Text: "2026-09-01", Time: day(1, 0, 0)}},
		{"until:2026-09-27T12:00:00Z", Value{Text: "2026-09-27T12:00:00Z", Time: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}},
		{"since:2026-09-27T14:00:00.5+02:00", Value{Text: "2026-09-27T14:00:00.5+02:00", Time: time.Date(2026, 9, 27, 12, 0, 0, 5e8, time.UTC)}},
		{"data.tool:os_exec", Value{Text: "os_exec"}},
		{"data.iter>=3", Value{Text: "3"}},
		{`data.path:"C:\Users\x"`, Value{Text: `C:\Users\x`, Quoted: true}},
		{`data.q:"say \"hi\" \\ bye"`, Value{Text: `say "hi" \ bye`, Quoted: true}},
	} {
		f := parse(t, c.in)
		if len(f.Terms) != 1 || len(f.Terms[0].Values) != 1 || !reflect.DeepEqual(f.Terms[0].Values[0], c.want) {
			t.Errorf("%s: got %+v, want %+v", c.in, f.Terms, c.want)
		}
	}
}

func TestParseTerms(t *testing.T) {
	f := parse(t, `  id:1,2,5-9  log:"a b",c*   -"permission denied" hello, data.req.method!=GET   `)
	if got := f.String(); got != `id:1,2,5-9 log:"a b",c* -"permission denied" hello, data.req.method!=GET` {
		t.Errorf("String = %q", got)
	}
	if v := f.Terms[0].Values; len(v) != 3 || v[2].Lo != 5 || v[2].Hi != 9 {
		t.Errorf("list: %+v", v)
	}
	if v := f.Terms[1].Values; len(v) != 2 || !v[0].Quoted || v[0].Glob || !v[1].Glob {
		t.Errorf("mixed list: %+v", v)
	}
	if tm := f.Terms[2]; !tm.Not || tm.Field != "" || tm.Values[0].Text != "permission denied" {
		t.Errorf("negated phrase: %+v", tm)
	}
	if tm := f.Terms[3]; tm.Field != "" || tm.Values[0].Text != "hello," {
		t.Errorf("word: %+v", tm)
	}
	if tm := f.Terms[4]; tm.Field != "data.req.method" || tm.Op != Ne {
		t.Errorf("data term: %+v", tm)
	}
	if f := parse(t, " \t "); f.Terms != nil {
		t.Errorf("blank filter: %+v", f)
	}
	// A word that isn't shaped like a field is text, even with a colon.
	if f := parse(t, "14:03 3:1"); f.Terms[0].Field != "" || f.Terms[1].Field != "" {
		t.Errorf("clock-like words: %+v", f)
	}
}

// Errors say where and what was expected.
func TestParseErrors(t *testing.T) {
	for _, c := range []struct {
		in   string
		pos  int
		want string
	}{
		{"levle>=warning", 0, `unknown field "levle"`},
		{"http://x", 0, "quote text to search"},
		{"log", -1, ""}, // a word, not an error
		{"level=>warning", 6, `not ">warning"`},
		{"log!discord", 3, "expected an operator after log"},
		{"log>=a", 3, "log takes : = or !=, not >="},
		{"since>=1h", 5, "since takes : or =, not >="},
		{"since!=1h", 5, "since takes : or =, not !="},
		{"log:", 4, "expected a value"},
		{"log:a,,b", 6, "expected a value"},
		{"log:a,", 6, "expected a value"},
		{`log:a"b"`, 5, "a quote inside a value"},
		{`log:"a"b`, 7, "expected , or a space"},
		{`log:"abc`, 4, "never closed"},
		{`"abc`, 0, "never closed"},
		{`"  "`, 0, "expected text inside the quotes"},
		{`"a"b`, 3, "expected a space after the closing quote"},
		{`ab"c"`, 2, "a quote inside a word"},
		{"- log:a", 0, "expected a field or text after -"},
		{"x -", 2, "expected a field or text after -"},
		{"--log:a", 0, "expected a field or text after -"},
		{"id:abc", 3, "id takes a number or a range"},
		{"id:4294967296", 3, "id takes a number"},
		{"id:-5", 3, "id takes a number"},
		{"id:9-5", 3, "runs backwards"},
		{"id>=5-9", 4, "one number, not a range"},
		{"id>5,6", 4, "id> takes one value, not a list"},
		{"pid:1-", 4, "pid takes a number"},
		{"level:loud", 6, "level takes critical, error"},
		{"channel:debug", 8, "channel takes operational or diagnostic"},
		{"since:soon", 6, "since takes a duration"},
		{"since:-5m", 6, "since takes a duration"},
		{"since:1h,2h", 8, "since: takes one value"},
		{"trace:3f2a*", 6, "trace takes a whole ID"},
		{"data.x>=a*", 8, "not a pattern"},
		{"data.:x", 0, "expected a key after data."},
		{"data.a..b:x", 0, "expected a key after data."},
		{"data:x", 0, `unknown field "data"`},
	} {
		_, err := ParseAt(c.in, now)
		if c.pos < 0 {
			if err != nil {
				t.Errorf("%q: %v", c.in, err)
			}
			continue
		}
		var se *SyntaxError
		if !errors.As(err, &se) || se.Pos != c.pos || !strings.Contains(se.Msg, c.want) {
			t.Errorf("%q: error %v, want at %d containing %q", c.in, err, c.pos, c.want)
		}
	}
	if _, err := ParseAt("log:", now); err == nil || !strings.HasPrefix(err.Error(), "filter: column 5: ") {
		t.Errorf("Error() = %v", err)
	}
}

// The appendix's examples, and catalog.Filter's form, parse.
func TestAppendixExamples(t *testing.T) {
	for _, s := range appendix {
		parse(t, s)
	}
}

var appendix = []string{
	`level>=warning since:24h`,
	`log:discord id:2001-2999 -id:2005`,
	`provider:mcp.* level>=error`,
	`trace:3f2a9c00000000000000000000000000`,
	`tag:audit.failure since:7d`,
	`data.tool:os_exec "permission denied"`,
	`boot:current level>=warning`,
	`provider:system id:3101`,
	`since:14:00 until:14:10`,
	`level>=warning log:discord id:2001-2999 -id:2005 since:24h "reset"`,
}

// roundTrips is the property: a filter that parses prints as text that
// parses back to the same filter, and prints the same again.
func roundTrips(t *testing.T, s string) bool {
	f, err := ParseAt(s, now)
	if err != nil {
		return true
	}
	printed := f.String()
	g, err := ParseAt(printed, now)
	if err != nil {
		t.Errorf("%q printed as %q, which doesn't parse: %v", s, printed, err)
		return false
	}
	if !reflect.DeepEqual(f, g) || g.String() != printed {
		t.Errorf("%q printed as %q, which parses to %+v, not %+v", s, printed, g, f)
		return false
	}
	return true
}

// soup is a random filter: mostly well-formed terms over tricky values, with
// some stray characters, so many parse and many don't.
type soup string

func (soup) Generate(r *rand.Rand, size int) reflect.Value {
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	value := func() string {
		v := pick("discord", "mcp.*", "a?b", "2001", "2001-2999", "5-9", "0", "4294967295", "warning", "ERR", "verbose",
			"15m", "2h", "7d", "1h30m", "14:03", "09:05:30", "today", "yesterday", "2026-09-01", "2026-09-27T14:03:00+02:00",
			"current", "operational", "diagnostic", "3.5", "true", "é", "x:y", "C:\\path", "", "-", "*")
		switch r.Intn(4) {
		case 0: // quoted, with escapes
			v = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v+pick("", " b", `"q"`, `\`, ",")) + `"`
		case 1:
			v += "," + pick("cron", "3000-3999", "info", `"a b"`, "")
		}
		return v
	}
	var b strings.Builder
	for range r.Intn(size%8 + 1) {
		b.WriteString(pick(" ", "  ", "\t"))
		switch r.Intn(5) {
		case 0:
			b.WriteString(pick("", `"`) + pick("reset", "permission denied", "hello,", "14:03", "a\\b", "") + pick("", `"`))
		default:
			b.WriteString(pick("", "", "-"))
			b.WriteString(pick("log", "provider", "id", "level", "channel", "task", "tag", "trace", "boot", "pid",
				"since", "until", "session", "data.tool", "data.req.method", "data.", "nope"))
			b.WriteString(pick(":", ":", "=", "!=", ">=", "<=", ">", "<", "!"))
			b.WriteString(value())
		}
		if r.Intn(10) == 0 {
			b.WriteString(pick(`"`, ",", "-", "\\", "*", ":"))
		}
	}
	return reflect.ValueOf(soup(b.String()))
}

// The round-trip property over random filters (§14).
func TestRoundTrip(t *testing.T) {
	parsed := 0
	prop := func(s soup) bool {
		if _, err := ParseAt(string(s), now); err == nil {
			parsed++
		}
		return roundTrips(t, string(s))
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
	if parsed < 500 {
		t.Errorf("only %d of 5000 random filters parsed; the generator no longer exercises the property", parsed)
	}
}

// FuzzRoundTrip is the same property over arbitrary text; go test runs its
// seeds, go test -fuzz explores.
func FuzzRoundTrip(f *testing.F) {
	for _, s := range appendix {
		f.Add(s)
	}
	f.Add(`id:1,2,5-9 log:"a b",c* -"permission denied" hello, data.q:"say \"hi\" \\"`)
	f.Fuzz(func(t *testing.T, s string) { roundTrips(t, s) })
}

// Filters from caller-built values print quoted where they must.
func TestStringQuotes(t *testing.T) {
	f := Filter{Terms: []Term{
		{Field: "actor", Op: Match, Values: []Value{{Text: "a b"}, {Text: "x,y"}, {Text: ""}}},
		{Values: []Value{{Text: "log:x"}}},
		{Values: []Value{{Text: "-x"}}},
	}}
	if got := f.String(); got != `actor:"a b","x,y","" "log:x" "-x"` {
		t.Errorf("String = %q", got)
	}
}
