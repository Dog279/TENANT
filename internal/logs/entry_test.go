package logs

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLine_RoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 22, 14, 3, 11, 123_000_000, time.Local)
	in := Entry{
		At: at, Source: "turns", Level: slog.LevelWarn, Msg: "tool result web_search\tfailed\nhard",
		Fields: []Field{
			{Key: "ev", Value: "tool_result"},
			{Key: "iter", Value: "2"},
			{Key: "args", Value: `{"q":"a b"}`},
			{Key: "text", Value: "line one\nline two"},
			{Key: "empty", Value: ""},
			{Key: "eq", Value: "a=b"},
			{Key: "uni", Value: "héllo → wörld"},
		},
	}
	line := in.Line()
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("Line must be single-line, got %q", line)
	}
	if !strings.HasPrefix(line, "2026-09-22 14:03:11.123 WARN  [turns] tool result web_search failed hard\t") {
		t.Fatalf("unexpected line prefix: %q", line)
	}
	out, ok := ParseLine(line)
	if !ok {
		t.Fatalf("ParseLine failed on %q", line)
	}
	if !out.At.Equal(at) || out.Source != "turns" || out.Level != slog.LevelWarn {
		t.Errorf("header mismatch: %+v", out)
	}
	if out.Msg != "tool result web_search failed hard" {
		t.Errorf("msg = %q", out.Msg)
	}
	if len(out.Fields) != len(in.Fields) {
		t.Fatalf("fields = %d, want %d: %+v", len(out.Fields), len(in.Fields), out.Fields)
	}
	for i, f := range in.Fields {
		if out.Fields[i].Key != f.Key || out.Fields[i].Value != f.Value {
			t.Errorf("field %d = %+v, want %+v", i, out.Fields[i], f)
		}
	}
	if out.Kind() != "tool_result" {
		t.Errorf("Kind() = %q", out.Kind())
	}
}

func TestParseLine_RejectsForeignFormats(t *testing.T) {
	for _, bad := range []string{
		"", "hello world", "2026-09-22 not a line", "2026-09-22 14:03:11.123 NOPE [x] m",
		"2026-09-22 14:03:11.123 INFO  no-source-bracket", "Sep 22 14:03:11 host syslog[1]: x",
	} {
		if _, ok := ParseLine(bad); ok {
			t.Errorf("ParseLine(%q) should fail", bad)
		}
	}
	// CRLF endings (a Windows script) parse fine.
	if e, ok := ParseLine("2026-09-22 14:03:11.123 INFO  [x] hi\r\n"); !ok || e.Msg != "hi" {
		t.Errorf("CRLF line should parse, got ok=%v e=%+v", ok, e)
	}
}

func TestNormalizeSource(t *testing.T) {
	cases := map[string]string{
		"discord": "discord", "My Script!": "my-script", "  Sub Agents ": "sub-agents",
		"..": "", "": "", ".hidden": "hidden", "a/b": "a-b", "UPPER_case.1": "upper_case.1",
		strings.Repeat("x", 60): strings.Repeat("x", 40),
	}
	for in, want := range cases {
		if got := NormalizeSource(in); got != want {
			t.Errorf("NormalizeSource(%q) = %q, want %q", in, got, want)
		}
	}
	// A source is a folder on Windows too: no device names, no trailing dot.
	for _, bad := range []string{"..", "a/b", "A", "-x", ".x", "", strings.Repeat("x", 41),
		"con", "prn", "aux", "nul", "com1", "com9", "lpt1", "lpt9", "nul.txt", "con.x.y", "com1.log", "x."} {
		if ValidSource(bad) {
			t.Errorf("ValidSource(%q) should be false", bad)
		}
	}
	for _, in := range []string{"CON", "Nul.txt", "LPT1"} {
		if got := NormalizeSource(in); got != "" {
			t.Errorf("NormalizeSource(%q) = %q, want refused", in, got)
		}
	}
	for _, ok := range []string{"console", "com10", "lpt", "auxiliary", "nul-x", "a.b"} {
		if !ValidSource(ok) {
			t.Errorf("ValidSource(%q) should be true", ok)
		}
	}
}

// The mirror escapes every control character in a message, so a line can't
// drive the terminal of a `tail -f`; CR, LF and TAB still become spaces, as
// v1 wrote them, and the line still parses. Values are quoted and escaped.
func TestLineEscapesControlCharacters(t *testing.T) {
	e := Entry{At: time.Date(2026, 9, 27, 9, 0, 0, 0, time.Local), Source: "x", Level: slog.LevelInfo,
		Msg: "a\x1b[31mred\x07 c1\u009b del\x7f bad\xff é\ttab\r\nnl", Fields: []Field{{Key: "k", Value: "v\x1b]0;t\x07"}, {Key: "c", Value: "a\u0085b"}}}
	line := e.Line()
	for _, r := range line {
		if r != '\t' && (r < ' ' || r == 0x7f || r >= 0x80 && r <= 0x9f) {
			t.Fatalf("control character %U reached the line %q", r, line)
		}
	}
	if !strings.Contains(line, `] a\x1b[31mred\a c1\u009b del\x7f bad\xff é tab nl`+"\t") {
		t.Errorf("message = %q", line)
	}
	got, ok := ParseLine(line)
	if !ok || got.Field("k") != "v\x1b]0;t\x07" || got.Field("c") != "a\u0085b" {
		t.Errorf("ParseLine(%q) = %+v, %v", line, got, ok)
	}
}

func TestCapValue_RuneSafe(t *testing.T) {
	v := "héllo wörld"
	got := capValue(v, 2) // 'h' + first byte of 'é' → must back up to 'h'
	if !strings.HasPrefix(got, "h…(+") {
		t.Errorf("capValue cut mid-rune: %q", got)
	}
	if capValue("short", 100) != "short" {
		t.Error("no cap expected")
	}
}

func TestLevelName_Buckets(t *testing.T) {
	if LevelName(slog.LevelInfo+2) != "INFO" || LevelName(slog.LevelError+4) != "ERROR" || LevelName(-1) != "DEBUG" {
		t.Error("custom levels must bucket into the four names")
	}
}
