package store

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
)

// The planner turns a query.Filter into a condition over events e (or, for
// statistics, over rollup r) and its arguments. User text only ever reaches
// SQL as an argument; full text is escaped into FTS5 strings, so its query
// syntax can't be injected either. Level conditions are written as literal
// numbers, so a warning-and-above filter carries `level <= 3` itself: the
// events_admin partial index applies only to a query that says so.

// Columns of the fields, by kind.
var (
	nameCols = map[string]string{"log": "log_id", "provider": "provider_id", "task": "task_id", "opcode": "opcode"} // names.id, by names.kind = the field
	textCols = map[string]string{"session": "session_id", "approval": "approval_id", "tool_call": "tool_call_id", "actor": "actor", "surface": "surface"}
	idCols   = map[string]string{"trace": "trace_id", "activity": "activity_id", "related": "related_id", "boot": "boot_id"}
	numCols  = map[string]string{"id": "event_id", "pid": "pid"}
)

// rollupable reports whether the rollups can answer f: they hold counts by
// minute, log and level only.
func rollupable(f query.Filter) bool {
	for _, t := range f.Terms {
		if t.Field != "log" && t.Field != "level" && t.Field != "since" && t.Field != "until" {
			return false
		}
	}
	return true
}

// where plans f over events e, or over rollup r when rollup is set (f must
// be rollupable; its times then round out to whole minutes).
func (s *Store) where(f query.Filter, rollup bool) (string, []any, error) {
	var conds []string
	var args []any
	var text []string // the positive full-text terms, matched together
	for _, t := range f.Terms {
		if len(t.Values) == 0 {
			return "", nil, errors.New("store: filter term " + strconv.Quote(t.Field) + " has no value")
		}
		if t.Field == "" && !t.Not {
			text = append(text, ftsString(t.Values[0].Text))
			continue
		}
		c, a, err := s.cond(t, rollup)
		if err != nil {
			return "", nil, err
		}
		conds, args = append(conds, c), append(args, a...)
	}
	if len(text) > 0 {
		conds = append(conds, "e.seq IN (SELECT rowid FROM events_fts WHERE events_fts MATCH ?)")
		args = append(args, strings.Join(text, " "))
	}
	if len(conds) == 0 {
		return "1", nil, nil
	}
	return strings.Join(conds, " AND "), args, nil
}

// cond plans one term.
func (s *Store) cond(t query.Term, rollup bool) (string, []any, error) {
	p, timeCol := "e.", "e.time_ns"
	if rollup {
		p, timeCol = "r.", "r.minute"
	}
	var c string
	var args []any
	switch f := t.Field; {
	case f == "level":
		return levels(t, p+"level"), nil, nil
	case f == "":
		c, args = "e.seq IN (SELECT rowid FROM events_fts WHERE events_fts MATCH ?)", []any{ftsString(t.Values[0].Text)}
	case f == "since" || f == "until":
		ns := t.Values[0].Time.UnixNano()
		c, args = timeCol+" >= ?", []any{ns}
		if f == "until" {
			c = timeCol + " < ?"
		}
		if rollup { // the minute a bound falls in counts whole
			m := ns / int64(time.Minute)
			if f == "until" && ns%int64(time.Minute) != 0 {
				m++
			}
			args[0] = m
		}
	case nameCols[f] != "":
		c, args = oneOf("name", t.Values)
		c = p + nameCols[f] + " IN (SELECT id FROM names WHERE kind = '" + f + "' AND " + c + ")"
	case f == "tag":
		c, args = oneOf("value", t.Values)
		c = "EXISTS (SELECT 1 FROM json_each(e.keywords) WHERE " + c + ")"
	case textCols[f] != "":
		c, args = oneOf("e."+textCols[f], t.Values)
	case idCols[f] != "":
		c = "e." + idCols[f] + " IN (?" + strings.Repeat(", ?", len(t.Values)-1) + ")"
		for _, v := range t.Values {
			switch {
			case f == "boot" && v.Text == "current" && len(s.boot) == 0:
				return "", nil, errors.New("store: boot:current: this store was opened without a boot ID")
			case f == "boot" && v.Text == "current":
				args = append(args, s.boot)
			default:
				args = append(args, idValue(v.Text))
			}
		}
	case f == "channel":
		c, args = numbers("e.channel", t)
	case numCols[f] != "":
		c, args = numbers("e."+numCols[f], t)
	case strings.HasPrefix(f, "data."):
		c, args = dataCond(strings.TrimPrefix(f, "data."), t)
	default:
		return "", nil, errors.New("store: unknown filter field " + strconv.Quote(f))
	}
	if t.Not != (t.Op == query.Ne) {
		c = "NOT coalesce((" + c + "), 0)" // a NULL column doesn't match, so its negation does
	}
	return c, args, nil
}

// oneOf is expr equal to one of vals, or matching one of their globs.
func oneOf(expr string, vals []query.Value) (string, []any) {
	var parts []string
	var args, exact []any
	for _, v := range vals {
		if v.Glob {
			parts, args = append(parts, expr+" GLOB ?"), append(args, v.Text)
		} else {
			exact = append(exact, v.Text)
		}
	}
	if len(exact) > 0 {
		parts, args = append(parts, expr+" IN (?"+strings.Repeat(", ?", len(exact)-1)+")"), append(args, exact...)
	}
	return anyOf(parts), args
}

// anyOf joins alternatives, parenthesized so the terms' AND can't split them.
func anyOf(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// numbers plans a numeric field: its numbers and ranges, or a comparison.
func numbers(col string, t query.Term) (string, []any) {
	switch t.Op {
	case query.Ge, query.Le, query.Gt, query.Lt:
		return col + " " + string(t.Op) + " ?", []any{t.Values[0].Lo}
	}
	var parts []string
	var args []any
	for _, v := range t.Values {
		if v.Lo == v.Hi {
			parts, args = append(parts, col+" = ?"), append(args, v.Lo)
		} else {
			parts, args = append(parts, col+" BETWEEN ? AND ?"), append(args, v.Lo, v.Hi)
		}
	}
	return anyOf(parts), args
}

// levels plans a level term as the set of levels it matches, written as
// literal numbers (they come from event.Level, never from the user). A set
// within warning-and-above carries `level <= 3`, the events_admin index's
// own condition.
func levels(t query.Term, col string) string {
	var in []string
	admin := true
	for l := int64(event.Critical); l <= int64(event.Verbose); l++ {
		hit := false
		for _, v := range t.Values {
			switch t.Op {
			case query.Ge: // at least as severe: a lower number
				hit = hit || l <= v.Lo
			case query.Gt:
				hit = hit || l < v.Lo
			case query.Le:
				hit = hit || l >= v.Lo
			case query.Lt:
				hit = hit || l > v.Lo
			default:
				hit = hit || l == v.Lo
			}
		}
		if hit != (t.Not != (t.Op == query.Ne)) {
			in = append(in, strconv.FormatInt(l, 10))
			admin = admin && l <= int64(event.Warning)
		}
	}
	switch {
	case len(in) == 0:
		return "0"
	case len(in) == 5:
		return "1"
	case admin && len(in) == 3:
		return col + " <= 3"
	case admin:
		return col + " <= 3 AND " + col + " IN (" + strings.Join(in, ", ") + ")"
	}
	return col + " IN (" + strings.Join(in, ", ") + ")"
}

// dataCond plans data.<key>. A dotted key names a flat field first (slog
// groups are flattened to dotted keys), then a path into an embedded JSON
// value. A number matches as a number and as text, and compares as a number;
// true and false match JSON's booleans too.
//
// A sensitive field never matches (nor, for a path, the field it is in):
// a filter would read what the viewers mask, without the audited reveal
// (§11). Its dtypes letter, at its place in data, is upper case.
func dataCond(key string, t query.Term) (string, []any) {
	expr, exprArgs := "json_extract(e.data, ?)", []any{`$."` + key + `"`}
	if strings.Contains(key, ".") {
		expr = "coalesce(json_extract(e.data, ?), json_extract(e.data, ?))"
		exprArgs = append(exprArgs, `$."`+strings.ReplaceAll(key, ".", `"."`)+`"`)
	}
	top, _, _ := strings.Cut(key, ".")
	masked := ` AND NOT EXISTS (SELECT 1 FROM (SELECT key, row_number() OVER (ORDER BY id) AS n FROM json_each(e.data))
		WHERE key IN (?, ?) AND substr(e.dtypes, n, 1) BETWEEN 'A' AND 'Z'))`
	switch t.Op {
	case query.Ge, query.Le, query.Gt, query.Lt:
		v := t.Values[0]
		return "(" + expr + " " + string(t.Op) + " ?" + masked, append(exprArgs, dataValues(v)[0], key, top)
	}
	var parts []string
	var args []any
	for _, v := range t.Values {
		if v.Glob {
			parts, args = append(parts, expr+" GLOB ?"), append(append(args, exprArgs...), v.Text)
			continue
		}
		alts := dataValues(v)
		c := expr + " IN (?" + strings.Repeat(", ?", len(alts)-1) + ")"
		if len(alts) == 1 && !strings.Contains(key, ".") {
			// Text is in data as the writer's JSON of it: finding that first
			// is a cheap test that skips parsing most rows' JSON.
			j, _ := json.Marshal(v.Text)
			c = "(instr(e.data, ?) > 0 AND " + c + ")"
			args = append(args, string(j[1:len(j)-1]))
		}
		parts = append(parts, c)
		args = append(append(args, exprArgs...), alts...)
	}
	return "(" + anyOf(parts) + masked, append(args, key, top)
}

// dataValues are what a data value may be stored as: its number first when
// it reads as one, and its text.
func dataValues(v query.Value) []any {
	if !v.Quoted {
		if n, err := strconv.ParseInt(v.Text, 10, 64); err == nil {
			return []any{n, v.Text}
		}
		if f, err := strconv.ParseFloat(v.Text, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return []any{f, v.Text}
		}
		switch v.Text {
		case "true":
			return []any{1, v.Text}
		case "false":
			return []any{0, v.Text}
		}
	}
	return []any{v.Text}
}

// ftsString is text as one FTS5 string: a phrase of its words, with none of
// FTS5's query syntax.
func ftsString(text string) string {
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}
