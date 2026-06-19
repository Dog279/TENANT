package toolfmt

import "strings"

// RepairJSON makes a best-effort repair of the common malformed-JSON shapes weak
// models emit: trailing commas, an unterminated trailing string, and missing
// closing brackets from truncation (the model ran out of tokens mid-object).
//
// It is a heuristic, not a parser: it returns its best attempt, and callers MUST
// still json.Unmarshal / json.Valid the result. Well-formed input is returned
// unchanged. It never panics. It is string-aware — commas, braces, and brackets
// INSIDE string values are preserved verbatim — and deliberately does NOT touch
// quoting style or missing keys, only the high-frequency, low-risk fixes.
//
// Shared so the structured-output jobs (distill/consolidate) can reuse it. (TEN-260)
func RepairJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	var stack []byte // expected closers, LIFO
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			b.WriteByte(c)
		case '{':
			stack = append(stack, '}')
			b.WriteByte(c)
		case '[':
			stack = append(stack, ']')
			b.WriteByte(c)
		case '}', ']':
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
			b.WriteByte(c)
		case ',':
			// Drop a trailing comma: if the next non-space char is a closer or
			// end-of-input, this comma is dangling — skip it.
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
				j++
			}
			if j >= len(s) || s[j] == '}' || s[j] == ']' {
				continue
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	out := b.String()
	if inStr {
		out += `"` // close an unterminated trailing string
	}
	// A comma can now be the last meaningful byte (e.g. `{"a":1, `); drop it
	// before appending the missing closers.
	out = strings.TrimRight(out, " \t\r\n,")
	for i := len(stack) - 1; i >= 0; i-- {
		out += string(stack[i])
	}
	return out
}
