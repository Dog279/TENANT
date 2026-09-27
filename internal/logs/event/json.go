package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// An event's data travels as two strings: a JSON object keyed by field name,
// in field order, and dtypes, one letter per field that JSON can't say:
//
//	s string, i int64, f float64, b bool, n nil, j JSON, t time, d duration
//
// upper case when the field is sensitive, so "sIt" is a string, a sensitive
// int64 and a time. A time is an RFC 3339 string (UTC, nanoseconds); a
// duration its nanoseconds. A value no type fits (a NaN, invalid JSON) is
// kept as its text. The store keeps these as its data and dtypes columns,
// and an event's JSON (MarshalJSON) carries them as they are.

// EncodeData renders fields as data and dtypes. Both are "" for no fields.
func EncodeData(fields []Field) (data, dtypes string) {
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
	return appendString(b, Field{Value: v}.Text()), 's'
}

// DecodeData reads data and dtypes back into fields. With dtypes "", each
// value's type is read from its JSON (a writer outside Tenant rarely sends
// dtypes): a string, an integral number as int64, any other number as
// float64, a bool, null, and an object or array as JSON. No field is
// sensitive then.
func DecodeData(data, dtypes string) ([]Field, error) {
	if data == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("event: data is not a JSON object")
	}
	infer := dtypes == ""
	out := make([]Field, 0, len(dtypes))
	for i := 0; dec.More(); i++ {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		f := Field{Key: tok.(string)}
		var code byte
		switch {
		case infer:
			code = inferType(raw)
		case i >= len(dtypes):
			return nil, errors.New("event: data has more fields than dtypes")
		default:
			code = dtypes[i]
		}
		if code >= 'A' && code <= 'Z' {
			f.Sensitive, code = true, code+('a'-'A')
		}
		if f.Value, err = decodeValue(raw, code); err != nil {
			return nil, fmt.Errorf("event: field %q: %w", f.Key, err)
		}
		out = append(out, f)
	}
	if !infer && len(out) != len(dtypes) {
		return nil, errors.New("event: dtypes has more fields than data")
	}
	return out, nil
}

func inferType(raw json.RawMessage) byte {
	switch s := strings.TrimSpace(string(raw)); {
	case s == "null":
		return 'n'
	case s == "true" || s == "false":
		return 'b'
	case strings.HasPrefix(s, `"`):
		return 's'
	case strings.HasPrefix(s, "{") || strings.HasPrefix(s, "["):
		return 'j'
	default:
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return 'i'
		}
		return 'f'
	}
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

// wire is an event's JSON: the form the API, exports and the spool carry.
type wire struct {
	Seq        uint64          `json:"seq,omitempty"`
	Time       time.Time       `json:"time"`
	Log        string          `json:"log"`
	Provider   string          `json:"provider,omitempty"`
	ID         uint32          `json:"id,omitempty"`
	Version    uint8           `json:"version,omitempty"`
	Level      string          `json:"level"`
	Channel    string          `json:"channel,omitempty"` // "diagnostic"; empty is operational
	Task       string          `json:"task,omitempty"`
	Opcode     string          `json:"opcode,omitempty"`
	Keywords   []string        `json:"keywords,omitempty"`
	Msg        string          `json:"msg"`
	Data       json.RawMessage `json:"data,omitempty"`
	DTypes     string          `json:"dtypes,omitempty"`
	Trace      string          `json:"trace,omitempty"`
	Activity   string          `json:"activity,omitempty"`
	Related    string          `json:"related,omitempty"`
	Session    string          `json:"session,omitempty"`
	Approval   string          `json:"approval,omitempty"`
	ToolCall   string          `json:"tool_call,omitempty"`
	Actor      string          `json:"actor,omitempty"`
	Surface    string          `json:"surface,omitempty"`
	PID        int             `json:"pid,omitempty"`
	Role       string          `json:"role,omitempty"`
	Boot       string          `json:"boot,omitempty"`
	Host       string          `json:"host,omitempty"`
	AppVersion string          `json:"app_version,omitempty"`
}

// MarshalJSON writes e in its wire form: named fields, the level and channel
// by name, and the data as the data/dtypes pair, so a round trip returns
// the same typed fields, sensitive flags included.
func (e Event) MarshalJSON() ([]byte, error) {
	data, dtypes := EncodeData(e.Data)
	w := wire{Seq: e.Seq, Time: e.Time, Log: e.Log, Provider: e.Provider, ID: e.ID, Version: e.Version,
		Level: e.Level.String(), Task: e.Task, Opcode: e.Opcode, Keywords: e.Keywords, Msg: e.Msg, DTypes: dtypes,
		Trace: e.Corr.TraceID, Activity: e.Corr.ActivityID, Related: e.Corr.RelatedID, Session: e.Corr.SessionID,
		Approval: e.Corr.ApprovalID, ToolCall: e.Corr.ToolCallID, Actor: e.Actor, Surface: e.Surface,
		PID: e.Proc.PID, Role: e.Proc.Role, Boot: e.Proc.BootID, Host: e.Proc.Host, AppVersion: e.Proc.Version}
	if data != "" {
		w.Data = json.RawMessage(data)
	}
	if e.Channel == Diagnostic {
		w.Channel = "diagnostic"
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads an event's wire form. A level it doesn't know (or
// none) is info; data without dtypes has its types read from its JSON.
func (e *Event) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	lvl, ok := ParseLevel(w.Level)
	if !ok {
		lvl = Info
	}
	data, err := DecodeData(string(w.Data), w.DTypes)
	if err != nil {
		return err
	}
	*e = Event{Seq: w.Seq, Time: w.Time, Log: w.Log, Provider: w.Provider, ID: w.ID, Version: w.Version,
		Level: lvl, Task: w.Task, Opcode: w.Opcode, Keywords: w.Keywords, Msg: w.Msg, Data: data,
		Corr:  Correlation{TraceID: w.Trace, ActivityID: w.Activity, RelatedID: w.Related, SessionID: w.Session, ApprovalID: w.Approval, ToolCallID: w.ToolCall},
		Actor: w.Actor, Surface: w.Surface,
		Proc: Process{PID: w.PID, Role: w.Role, BootID: w.Boot, Host: w.Host, Version: w.AppVersion}}
	if w.Channel == "diagnostic" {
		e.Channel = Diagnostic
	}
	return nil
}
