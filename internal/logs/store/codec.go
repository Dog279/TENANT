package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"tenant/internal/logs/event"
)

const insertEvent = `INSERT INTO events (log_id, provider_id, event_id, version, time_ns, level, channel,
  task_id, opcode, keywords, msg, data, dtypes, trace_id, activity_id, related_id,
  session_id, approval_id, tool_call_id, actor, surface, pid, role, boot_id, host, proc_version, bytes)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const selectEvents = `SELECT e.seq, l.name, p.name, e.event_id, e.version, e.time_ns, e.level, e.channel,
  coalesce(t.name, ''), coalesce(o.name, ''), coalesce(e.keywords, ''), e.msg, coalesce(e.data, ''), coalesce(e.dtypes, ''),
  e.trace_id, e.activity_id, e.related_id,
  coalesce(e.session_id, ''), coalesce(e.approval_id, ''), coalesce(e.tool_call_id, ''),
  coalesce(e.actor, ''), coalesce(e.surface, ''),
  coalesce(e.pid, 0), coalesce(r.name, ''), e.boot_id, coalesce(h.name, ''), coalesce(v.name, '')
FROM events e
JOIN names l ON l.id = e.log_id
JOIN names p ON p.id = e.provider_id
LEFT JOIN names t ON t.id = e.task_id
LEFT JOIN names o ON o.id = e.opcode
LEFT JOIN names r ON r.id = e.role
LEFT JOIN names h ON h.id = e.host
LEFT JOIN names v ON v.id = e.proc_version`

// row is an event as its columns hold it.
type row struct {
	logID, providerID                  int64
	eventID, version                   int64
	timeNS                             int64
	level, channel                     int64
	task, opcode                       any // names.id or NULL
	keywords, data, dtypes             any // text or NULL
	msg                                string
	trace, activity, related           any
	session, approval, toolCall        any
	actor, surface                     any
	pid, role, boot, host, procVersion any
	bytes                              int64
}

func (r *row) args() []any {
	return []any{r.logID, r.providerID, r.eventID, r.version, r.timeNS, r.level, r.channel,
		r.task, r.opcode, r.keywords, r.msg, r.data, r.dtypes, r.trace, r.activity, r.related,
		r.session, r.approval, r.toolCall, r.actor, r.surface, r.pid, r.role, r.boot, r.host, r.procVersion, r.bytes}
}

// interner resolves names to ids inside one commit. Names it adds join the
// store's cache only once the commit succeeds.
type interner struct {
	s     *Store
	tx    *sql.Tx
	ctx   context.Context
	added map[string]int64
}

func (in *interner) id(kind, name string) (int64, error) {
	k := kind + "\x00" + name
	if id, ok := in.s.names[k]; ok {
		return id, nil
	}
	if id, ok := in.added[k]; ok {
		return id, nil
	}
	if _, err := in.tx.ExecContext(in.ctx, `INSERT OR IGNORE INTO names (kind, name) VALUES (?, ?)`, kind, name); err != nil {
		return 0, err
	}
	var id int64
	if err := in.tx.QueryRowContext(in.ctx, `SELECT id FROM names WHERE kind = ? AND name = ?`, kind, name).Scan(&id); err != nil {
		return 0, err
	}
	in.added[k] = id
	return id, nil
}

// optID is id for a name that may be absent: NULL when it is.
func (in *interner) optID(kind, name string) (any, error) {
	if name == "" {
		return nil, nil
	}
	return in.id(kind, name)
}

// row encodes e.
func (in *interner) row(e *event.Event) (*row, error) {
	r := &row{eventID: int64(e.ID), version: int64(e.Version), timeNS: e.Time.UnixNano(),
		level: int64(e.Level), channel: int64(e.Channel), msg: e.Msg,
		trace: idValue(e.Corr.TraceID), activity: idValue(e.Corr.ActivityID), related: idValue(e.Corr.RelatedID),
		session: text(e.Corr.SessionID), approval: text(e.Corr.ApprovalID), toolCall: text(e.Corr.ToolCallID),
		actor: text(e.Actor), surface: text(e.Surface), boot: idValue(e.Proc.BootID)}
	var err error
	if r.logID, err = in.id("log", e.Log); err != nil {
		return nil, err
	}
	if r.providerID, err = in.id("provider", e.Provider); err != nil {
		return nil, err
	}
	for _, n := range []struct {
		dst        *any
		kind, name string
	}{{&r.task, "task", e.Task}, {&r.opcode, "opcode", e.Opcode}, {&r.role, "role", e.Proc.Role},
		{&r.host, "host", e.Proc.Host}, {&r.procVersion, "version", e.Proc.Version}} {
		if *n.dst, err = in.optID(n.kind, n.name); err != nil {
			return nil, err
		}
	}
	if e.Proc.PID != 0 {
		r.pid = int64(e.Proc.PID)
	}
	if len(e.Keywords) > 0 {
		b, _ := json.Marshal(e.Keywords)
		r.keywords = string(b)
	}
	if data, types := encodeData(e.Data); data != "" {
		r.data, r.dtypes = data, types
		r.bytes = int64(len(data) + len(types))
	}
	r.bytes += int64(len(e.Msg) + len(e.Actor) + len(e.Surface) + len(e.Corr.SessionID) + len(e.Corr.ApprovalID) + len(e.Corr.ToolCallID))
	return r, nil
}

func text(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// idValue is an ID's column value: 16 bytes for runtrace's 32 lowercase hex
// characters, the text itself otherwise.
func idValue(s string) any {
	if s == "" {
		return nil
	}
	if b := idBytes(s); len(b) == 16 && len(s) == 32 {
		return b
	}
	return s
}

// idBytes is s as 16 bytes when it is 32 lowercase hex characters, else its
// bytes.
func idBytes(s string) []byte {
	if len(s) == 32 {
		if b, err := hex.DecodeString(s); err == nil && hex.EncodeToString(b) == s {
			return b
		}
	}
	return []byte(s)
}

// idText reads an ID column back.
func idText(v any) string {
	switch x := v.(type) {
	case []byte:
		if len(x) == 16 {
			return hex.EncodeToString(x)
		}
		return string(x)
	case string:
		return x
	}
	return ""
}

// encodeData renders fields as the data and dtypes columns (see the package
// doc). Both are "" when there are no fields.
func encodeData(fields []event.Field) (data, types string) {
	if len(fields) == 0 {
		return "", ""
	}
	b := make([]byte, 0, 64*len(fields))
	t := make([]byte, 0, len(fields))
	b = append(b, '{')
	for i, f := range fields {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, f.Key)
		b = append(b, ':')
		var code byte
		b, code = appendValue(b, f.Value)
		if f.Sensitive {
			code -= 'a' - 'A'
		}
		t = append(t, code)
	}
	return string(append(b, '}')), string(t)
}

func appendString(b []byte, s string) []byte {
	j, _ := json.Marshal(s) // a string always marshals
	return append(b, j...)
}

// appendValue appends v as JSON and returns its type letter.
func appendValue(b []byte, v any) ([]byte, byte) {
	switch x := v.(type) {
	case string:
		return appendString(b, x), 's'
	case int64:
		return strconv.AppendInt(b, x, 10), 'i'
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return strconv.AppendFloat(b, x, 'g', -1, 64), 'f'
		}
	case bool:
		return strconv.AppendBool(b, x), 'b'
	case nil:
		return append(b, "null"...), 'n'
	case json.RawMessage:
		if json.Valid(x) {
			return append(b, x...), 'j'
		}
	case time.Time:
		return appendString(b, x.UTC().Format(time.RFC3339Nano)), 't'
	case time.Duration:
		return strconv.AppendInt(b, int64(x), 10), 'd'
	}
	return appendString(b, event.Field{Value: v}.Text()), 's'
}

// decodeData reads the data and dtypes columns back into fields.
func decodeData(data, types string) ([]event.Field, error) {
	if data == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("data is not a JSON object")
	}
	out := make([]event.Field, 0, len(types))
	for i := 0; dec.More(); i++ {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if i >= len(types) {
			return nil, errors.New("data has more fields than dtypes")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		f := event.Field{Key: tok.(string)}
		code := types[i]
		if code >= 'A' && code <= 'Z' {
			f.Sensitive, code = true, code+('a'-'A')
		}
		if f.Value, err = decodeValue(raw, code); err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Key, err)
		}
		out = append(out, f)
	}
	if len(out) != len(types) {
		return nil, errors.New("dtypes has more fields than data")
	}
	return out, nil
}

func decodeValue(raw json.RawMessage, code byte) (any, error) {
	switch code {
	case 's':
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	case 'i':
		return strconv.ParseInt(string(raw), 10, 64)
	case 'f':
		return strconv.ParseFloat(string(raw), 64)
	case 'b':
		return string(raw) == "true", nil
	case 'n':
		return nil, nil
	case 'j':
		return json.RawMessage(append([]byte(nil), raw...)), nil
	case 't':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return time.Parse(time.RFC3339Nano, s)
	case 'd':
		n, err := strconv.ParseInt(string(raw), 10, 64)
		return time.Duration(n), err
	}
	return nil, fmt.Errorf("unknown type %q", code)
}

type scanner interface{ Scan(dest ...any) error }

func scanEvent(sc scanner) (event.Event, error) {
	var e event.Event
	var id, version, timeNS, level, channel int64
	var keywords, data, types string
	var trace, activity, related, boot any
	err := sc.Scan(&e.Seq, &e.Log, &e.Provider, &id, &version, &timeNS, &level, &channel,
		&e.Task, &e.Opcode, &keywords, &e.Msg, &data, &types, &trace, &activity, &related,
		&e.Corr.SessionID, &e.Corr.ApprovalID, &e.Corr.ToolCallID, &e.Actor, &e.Surface,
		&e.Proc.PID, &e.Proc.Role, &boot, &e.Proc.Host, &e.Proc.Version)
	if err != nil {
		return e, err
	}
	e.ID, e.Version, e.Level, e.Channel = uint32(id), uint8(version), event.Level(level), event.Channel(channel)
	e.Time = time.Unix(0, timeNS).UTC()
	e.Corr.TraceID, e.Corr.ActivityID, e.Corr.RelatedID, e.Proc.BootID = idText(trace), idText(activity), idText(related), idText(boot)
	if keywords != "" {
		if err := json.Unmarshal([]byte(keywords), &e.Keywords); err != nil {
			return e, fmt.Errorf("event %d: keywords: %w", e.Seq, err)
		}
	}
	if e.Data, err = decodeData(data, types); err != nil {
		return e, fmt.Errorf("event %d: data: %w", e.Seq, err)
	}
	return e, nil
}
