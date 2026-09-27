// Package event is the logging v2 event model (docs/design/logging-v2.md):
// the typed, catalogued record the log book keeps, and the API that writes
// one where the action happens. It imports nothing from the module, so every
// package may import it.
//
// A provider declares its events once, at package init, and call sites emit
// by definition:
//
//	var Cron = event.NewProvider("cron", "cron")
//	var JobFailed = Cron.Event(3001, event.Error, "Job run", "Job {job} failed: {err}", "job", "err").
//		Doc("A scheduled job failed.", "Its command or prompt returned an error.", "Open the job's run in the cron log.")
//
//	JobFailed.Log(ctx, "job", name, "err", err)
//
// Event IDs are unique within their provider and never reused: a retired ID
// stays in the catalog, marked Retired. They are assigned in blocks by task:
//
//	1–999       lifecycle: starting, ready, stopping, stopped, degraded, recovered
//	1000–1999   normal operations (the provider's main tasks)
//	2000–2999   warnings, retries and degradation
//	3000–3999   errors and failures
//	4000–4999   security and audit (in security, and in providers that write audit events)
//	5000–5999   configuration changes
//	9000–9999   diagnostics (the diagnostic channel)
//
// internal/logs installs itself as the Emitter at start-up. Before that, Log
// sends warnings and worse to slog.Default and drops the rest, and Audit
// returns ErrNoEmitter.
package event

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Level is an event's severity. As in Event Viewer, a lower number is more
// severe: warning and worse is Level <= Warning.
type Level int8

const (
	Critical Level = 1 + iota // a crash, a lost store, a broken audit chain
	Error
	Warning
	Info
	Verbose // debug output
)

var levelNames = [...]string{Critical: "critical", Error: "error", Warning: "warning", Info: "info", Verbose: "verbose"}

func (l Level) String() string {
	if l >= Critical && l <= Verbose {
		return levelNames[l]
	}
	return "level(" + strconv.Itoa(int(l)) + ")"
}

// ParseLevel reads a level name, case-insensitively. It also takes err, warn
// and debug, the names v1 and slog use.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return Critical, true
	case "error", "err":
		return Error, true
	case "warning", "warn":
		return Warning, true
	case "info":
		return Info, true
	case "verbose", "debug":
		return Verbose, true
	}
	return 0, false
}

// Slog is the slog level l is written at. Critical sits above ERROR, where
// logs.LevelCritical is.
func (l Level) Slog() slog.Level {
	switch l {
	case Critical:
		return slog.LevelError + 4
	case Error:
		return slog.LevelError
	case Warning:
		return slog.LevelWarn
	case Verbose:
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// LevelFromSlog buckets a slog level into the five.
func LevelFromSlog(l slog.Level) Level {
	switch {
	case l >= slog.LevelError+4:
		return Critical
	case l >= slog.LevelError:
		return Error
	case l >= slog.LevelWarn:
		return Warning
	case l >= slog.LevelInfo:
		return Info
	}
	return Verbose
}

// Channel separates the always-on record from high-volume detail.
type Channel uint8

const (
	Operational Channel = iota // always on, and mirrored to the v1 text files
	Diagnostic                 // tool payloads, per-call metrics, requests: kept briefly
)

// The opcodes of an activity's events. A plain event has none.
const (
	OpStart = "start"
	OpStop  = "stop"
	OpFail  = "fail"
)

// Field is one typed value of an event's data. Value is a string, int64,
// float64, bool, time.Time (UTC), time.Duration, json.RawMessage or nil: Log
// converts whatever it is given to one of those.
type Field struct {
	Key       string
	Value     any
	Sensitive bool // personal content or a payload: kept, masked in viewers, never in Msg or the text mirror
}

// Masked stands in for a sensitive value wherever text is written: the
// mirror, the fallback output.
const Masked = "«sensitive»"

// Text is the value as the message and the text mirror show it.
func (f Field) Text() string {
	switch v := f.Value.(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case time.Duration:
		return v.String()
	case json.RawMessage:
		return string(v)
	case nil:
		return ""
	}
	return fmt.Sprint(f.Value)
}

// value converts a value given to Log to one of Field's types: any integer
// becomes an int64 (a uint64 past MaxInt64, its decimal text), a NaN or an
// infinity its text (JSON can't carry them), an error its message, a Stringer
// its text, and anything else with no type of its own its JSON.
func value(v any) any {
	switch x := v.(type) {
	case nil, string, int64, bool, time.Duration, json.RawMessage:
		return x
	case float64:
		return finite(x)
	case time.Time:
		return x.UTC()
	case int:
		return int64(x)
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case []byte:
		return string(x)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if u := rv.Uint(); u <= math.MaxInt64 {
			return int64(u)
		}
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return finite(rv.Float())
	}
	if b, err := json.Marshal(v); err == nil {
		return json.RawMessage(b)
	}
	return fmt.Sprint(v)
}

func finite(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return f
}

// Correlation links an event to the work that caused it. TraceID, ActivityID
// and RelatedID are 32 lowercase hex characters, like runtrace.NewID's.
type Correlation struct {
	TraceID    string // the turn (runtrace)
	ActivityID string // the operation the event belongs to (Start)
	RelatedID  string // on an activity's start event: the activity or turn that started it
	SessionID  string
	ApprovalID string
	ToolCallID string
}

// Process is the process that wrote an event.
type Process struct {
	PID     int
	Role    string // serve, tui, cli or child
	BootID  string // runtrace.BootID
	Host    string
	Version string
}

// Event is one record of the log book.
type Event struct {
	Seq            uint64 // assigned by the store
	Log, Provider  string
	ID             uint32 // within its provider; 0 = not yet catalogued
	Version        uint8
	Time           time.Time // UTC
	Level          Level
	Channel        Channel
	Task, Opcode   string
	Keywords       []string // shared with its definition: never modify it in place
	Msg            string   // rendered from the template's non-sensitive fields
	Data           []Field
	Corr           Correlation
	Actor, Surface string
	Proc           Process
}

// Emitter files events. internal/logs installs one at start-up.
type Emitter interface {
	// Emit files e without blocking: it queues it, or drops and counts it.
	// It owns e from then on.
	Emit(ctx context.Context, e *Event)
	// Audit files e and returns once its record is committed, or with the
	// error that stopped it.
	Audit(ctx context.Context, e *Event) error
}

var emitter atomic.Pointer[Emitter]

// Install makes em the emitter every definition writes through; nil
// uninstalls it.
func Install(em Emitter) {
	if em == nil {
		emitter.Store(nil)
		return
	}
	emitter.Store(&em)
}

// ErrNoEmitter is Audit's error before an emitter is installed: the record
// was not written.
var ErrNoEmitter = errors.New("event: no emitter installed; the record was not written")

// Log writes the event, from kv's alternating keys and values. It never
// blocks. A key d does not declare is kept, but never rendered into Msg.
func (d *Def) Log(ctx context.Context, kv ...any) { emit(ctx, d.build(ctx, "", kv)) }

// Line writes an event no definition describes (ID 0) into log, with msg as
// its message and kv as its fields, as Log takes them: text Tenant files but
// did not write, such as a line a child process printed, which its parent
// files under the child's log (internal/logs/capture). It never blocks.
func Line(ctx context.Context, log string, lvl Level, msg string, kv ...any) {
	d := Def{spec: Spec{Provider: log, Log: log, Level: lvl}}
	e := d.build(ctx, "", kv)
	e.Msg = msg
	emit(ctx, e)
}

// Audit writes the event like Log, but returns only once its record is
// committed, or with the error that stopped it: for records that must not be
// lost, such as the Security log's.
func (d *Def) Audit(ctx context.Context, kv ...any) error {
	em := emitter.Load()
	if em == nil {
		return ErrNoEmitter
	}
	return (*em).Audit(ctx, d.build(ctx, "", kv))
}

// badKey files a value that has no key, as slog does.
const badKey = "!BADKEY"

// build makes the event d describes, correlated with the activity in ctx.
func (d *Def) build(ctx context.Context, op string, kv []any) *Event {
	s := &d.spec
	e := &Event{
		Log: s.Log, Provider: s.Provider, ID: s.ID, Time: time.Now().UTC(),
		Level: s.Level, Channel: s.Channel, Task: s.Task, Opcode: op, Keywords: s.Keywords,
		Data: make([]Field, 0, (len(kv)+1)/2),
		Corr: Correlation{ActivityID: ActivityFromContext(ctx)},
	}
	for len(kv) > 0 {
		key, ok := kv[0].(string)
		if !ok || len(kv) == 1 {
			e.Data = append(e.Data, Field{Key: badKey, Value: value(kv[0])})
			kv = kv[1:]
			continue
		}
		e.Data = append(e.Data, Field{Key: key, Value: value(kv[1]), Sensitive: slices.Contains(s.Sensitive, key)})
		kv = kv[2:]
	}
	e.Msg = d.render(e.Data)
	return e
}

// emit hands e to the emitter. With none installed, warnings and worse go to
// slog.Default, sensitive values masked, and the rest is dropped.
func emit(ctx context.Context, e *Event) {
	if em := emitter.Load(); em != nil {
		(*em).Emit(ctx, e)
		return
	}
	if e.Level > Warning {
		return
	}
	attrs := make([]slog.Attr, 0, len(e.Data)+3)
	attrs = append(attrs, slog.String("log", e.Log), slog.String("event", e.Provider+"/"+strconv.FormatUint(uint64(e.ID), 10)))
	if e.Opcode != "" {
		attrs = append(attrs, slog.String("opcode", e.Opcode))
	}
	for _, f := range e.Data {
		if f.Sensitive {
			attrs = append(attrs, slog.String(f.Key, Masked))
		} else {
			attrs = append(attrs, slog.Any(f.Key, f.Value))
		}
	}
	slog.Default().LogAttrs(ctx, e.Level.Slog(), e.Msg, attrs...)
}

type activityKey struct{}

// ActivityFromContext is the ID of the activity ctx runs under ("" when none).
func ActivityFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(activityKey{}).(string)
	return id
}

// Activity is an operation in flight, from Start to End.
type Activity struct {
	d     *Def
	ctx   context.Context // End's events are correlated from it
	kv    []any
	start time.Time
}

// Start begins an activity: it mints the activity's ID, emits d's start
// event, with RelatedID set to the activity ctx already runs under (the
// emitter fills in the turn when there is none), and returns a context
// carrying the new activity, so the events written under it belong to it.
//
//	ctx, act := event.Start(ctx, catalog.JobRun, "job", name)
//	defer act.End(&err)
func Start(ctx context.Context, d *Def, kv ...any) (context.Context, *Activity) {
	start := time.Now()
	parent := ActivityFromContext(ctx)
	ctx = context.WithValue(ctx, activityKey{}, newID())
	e := d.build(ctx, OpStart, kv)
	e.Corr.RelatedID = parent
	emit(ctx, e)
	return ctx, &Activity{d: d, ctx: ctx, kv: kv, start: start}
}

// End emits the activity's stop event, with its duration; or, when *errp
// holds an error, its fail event, with the error too, at Error or worse.
func (a *Activity) End(errp *error) {
	kv := append(a.kv[:len(a.kv):len(a.kv)], "duration", time.Since(a.start))
	op := OpStop
	var err error
	if errp != nil {
		err = *errp
	}
	if err != nil {
		op = OpFail
		kv = append(kv, "error", err)
	}
	e := a.d.build(a.ctx, op, kv)
	if err != nil && e.Level > Error {
		e.Level = Error
	}
	emit(a.ctx, e)
}

// newID mints a 128-bit activity ID, in runtrace.NewID's form.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails (Go 1.24+)
	return hex.EncodeToString(b[:])
}
