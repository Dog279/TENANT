package logs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"tenant/internal/runtrace"
)

// defaultHub is the process-wide hub For() loggers resolve at write time, so a
// package can build its logger at init and have it start filing the moment
// the hub is installed.
var defaultHub atomic.Pointer[Hub]

// fallback catches records from For() loggers while no hub is installed
// (one-shot commands, tests): stderr at WARN, like the historical CLI logger,
// unless SetFallback swapped in another handler.
var fallback atomic.Pointer[slog.Handler]

var stderrWarn slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})

// SetFallback routes For loggers through h while no hub is installed; nil
// restores stderr at WARN. A one-shot command hands it a handler that files
// its warnings in the log book as well as printing them.
func SetFallback(h slog.Handler) {
	if h == nil {
		fallback.Store(nil)
		return
	}
	fallback.Store(&h)
}

func fallbackHandler() slog.Handler {
	if p := fallback.Load(); p != nil {
		return *p
	}
	return stderrWarn
}

// Install makes h the hub that For loggers (and anything else asking for
// Default) write to, and the emitter of catalogued events (emit.go). Call
// once at startup, right after Open.
func Install(h *Hub) {
	defaultHub.Store(h)
	installEmitter(h)
}

// Default returns the installed hub, or nil before Install.
func Default() *Hub { return defaultHub.Load() }

// For returns a logger whose every record is filed under the named source —
// the copy-paste hook for a new subsystem:
//
//	var log = logs.For("myfeature")
//	log.Info("started", "port", 8080)   // → <data>/logs/myfeature/<day>.log + a "myfeature" tab
//
// It is safe before Install (records go to stderr at WARN until a hub exists)
// and needs no plumbing: no handle to pass around, nothing to register.
func For(source string) *slog.Logger { return slog.New(&Handler{source: source}) }

// Handler is a slog.Handler that files records into a Hub. The source is the
// handler's fixed source (Hub.Logger / For), else a `source` attribute on the
// record or logger, else derived from the message's "subsystem: …" prefix
// (see prefixTable), else DefaultSource. An optional tee handler (stderr, a
// file) receives every record too, filtered by its own level — so a daemon
// keeps its supervisor-captured stderr exactly as before while the folders
// gain the full record.
type Handler struct {
	hub    *Hub         // nil ⇒ Default() at handle time
	tee    slog.Handler // optional second sink
	source string       // fixed source ("" ⇒ derive per record)
	attrs  []slog.Attr  // accumulated With attrs (keys already group-prefixed)
	prefix string       // current WithGroup key prefix ("" or "a.b.")
}

// Handler returns a slog.Handler writing into h, teeing to tee when non-nil.
func (h *Hub) Handler(tee slog.Handler) *Handler { return &Handler{hub: h, tee: tee} }

// Logger returns a logger pinned to one source of h.
func (h *Hub) Logger(source string) *slog.Logger { return slog.New(&Handler{hub: h, source: source}) }

func (h *Handler) resolve() *Hub {
	if h.hub != nil {
		return h.hub
	}
	return Default()
}

// Enabled: a record is worth building when the hub would keep it or the tee
// would print it.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	if h.tee != nil && h.tee.Enabled(ctx, l) {
		return true
	}
	hub := h.resolve()
	if hub == nil {
		return fallbackHandler().Enabled(ctx, l)
	}
	return l >= hub.opts.MinLevel
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if h.tee != nil && h.tee.Enabled(ctx, r.Level) {
		_ = h.tee.Handle(ctx, r)
	}
	hub := h.resolve()
	if hub == nil {
		fb := fallbackHandler()
		if !fb.Enabled(ctx, r.Level) {
			return nil
		}
		if h.source != "" { // the fallback may file it too: keep the folder
			fb = fb.WithAttrs([]slog.Attr{slog.String("source", h.source)})
		}
		if len(h.attrs) > 0 {
			fb = fb.WithAttrs(h.attrs)
		}
		return fb.Handle(ctx, r)
	}
	if r.Level < hub.opts.MinLevel {
		return nil
	}
	src := h.source
	fields := make([]Field, 0, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		addAttr(&fields, &src, h.source == "", a, "")
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(&fields, &src, h.source == "", a, h.prefix)
		return true
	})
	// A …Context call inside a turn joins the line to its run record.
	if id := runtrace.IDFromContext(ctx); id != "" {
		fields = append(fields, Field{Key: "trace", Value: id})
	}
	if src == "" {
		src = prefixSource(r.Message)
	}
	hub.Log(Entry{At: r.Time, Source: src, Level: r.Level, Msg: r.Message, Fields: fields})
	return nil
}

// addAttr flattens one attribute into fields (groups become dotted keys). A
// top-level `source` attribute names the folder when the handler has no fixed
// source (allowSource) and is consumed rather than filed.
func addAttr(fields *[]Field, src *string, allowSource bool, a slog.Attr, prefix string) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, g := range a.Value.Group() {
			addAttr(fields, src, allowSource, g, p)
		}
		return
	}
	if allowSource && prefix == "" && a.Key == "source" {
		if s := NormalizeSource(a.Value.String()); s != "" {
			*src = s
		}
		return
	}
	*fields = append(*fields, Field{Key: prefix + a.Key, Value: attrString(a.Value)})
}

func attrString(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return x.Error()
		case fmt.Stringer:
			return x.String()
		case []byte:
			return string(x)
		}
		return fmt.Sprint(v.Any())
	}
	return v.String()
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	n := *h
	n.attrs = make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	n.attrs = append(n.attrs, h.attrs...)
	for _, a := range attrs {
		if h.prefix != "" && a.Value.Kind() != slog.KindGroup {
			a.Key = h.prefix + a.Key
		} else if h.prefix != "" {
			a = slog.Attr{Key: strings.TrimSuffix(h.prefix, ".") + "." + a.Key, Value: a.Value}
		}
		n.attrs = append(n.attrs, a)
	}
	if h.tee != nil {
		n.tee = h.tee.WithAttrs(attrs)
	}
	return &n
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.prefix = h.prefix + name + "."
	if h.tee != nil {
		n.tee = h.tee.WithGroup(name)
	}
	return &n
}

// prefixTable maps the "<tag>: …" message conventions already used across the
// code base to their folders, so every existing log line lands in a sensible
// place without touching the call sites. Unknown tags fall to DefaultSource
// rather than minting folders from arbitrary message text.
var prefixTable = map[string]string{
	"mcp": "mcp", "mcp-memory": "mcp", "mcpserver": "mcp", "mcpremote": "mcp", "gateway": "mcp",
	"route": "route", "router": "model", "vllm": "model", "anthropic": "model", "claudecode": "model",
	"openai": "model", "ollama": "model", "model": "model", "fallback": "model", "echo": "model",
	"improve": "improve", "reflect": "improve", "distill": "improve", "consolidate": "improve",
	"soul-nudge": "improve", "souljob": "improve", "sme": "improve", "skill-induction": "improve",
	"profile": "improve", "feedback": "improve",
	"memory": "memory", "memory_search": "memory", "wiki": "memory", "archive": "memory",
	"skills": "memory", "recall": "memory", "compact": "memory", "episodic": "memory", "semantic": "memory",
	"peer": "peering", "peering": "peering", "federation": "peering",
	"relay": "discord", "discord": "discord",
	"imessage": "imessage", "bluebubbles": "imessage", "imsg": "imessage",
	"cron": "cron",
	"eval": "eval", "judge": "eval",
	"orchestra": "subagents", "team": "subagents", "spawn": "subagents", "research": "subagents",
	"dashboard": "dashboard", "tailscale": "dashboard", "ws": "dashboard",
	"approval": "approvals", "approvals": "approvals",
	"agent": "turns", "turn": "turns",
	"serve": "system", "setup": "system", "settings": "system", "tui": "system", "doctor": "system",
	"usage": "system", "tools": "system", "toolmux": "system",
}

// prefixSource derives a source from a "tag: message" prefix ("" when the
// message has none, or the tag is not a known subsystem).
func prefixSource(msg string) string {
	i := strings.IndexByte(msg, ':')
	if i <= 0 || i > 24 {
		return ""
	}
	tag := strings.ToLower(msg[:i])
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return ""
		}
	}
	return prefixTable[tag]
}

// SourceForMessage is prefixSource for callers outside the package (the CLI's
// bare `tenant log "<msg>"` form).
func SourceForMessage(msg string) string { return prefixSource(msg) }
