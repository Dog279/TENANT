// Package tenantlog is the copy-paste client for Tenant's log book.
//
// Copy THIS ONE FILE into any Go program — a sidecar, a watcher, a cron job, a
// custom automation on the box — and it logs into Tenant's unified log book:
//
//   - when a hub (`tenant serve` / `tenant tui --dashboard`) is reachable, each
//     line goes through the live logging API (POST /api/logs/<source>) and shows
//     up in the dashboard's Activity feed instantly;
//   - otherwise it is appended to the day's file, <data>/logs/<source>/YYYY-MM-DD.log,
//     in the exact on-disk format Tenant writes, and a hub that is (or comes) up
//     discovers the folder and tails it within seconds.
//
// Either way your program gets its own folder on disk and its own tab on the
// dashboard. Nothing in tenant has to be rebuilt or registered.
//
// Zero config on a Tenant box: the hub address and bearer token are read from
// Tenant's own config dir (config.json, else the dashboard-auth-token file a
// default install keeps the token in), the data dir is Tenant's OS data dir.
// Override any of it with Config, or with the env vars TENANT_URL,
// TENANT_TOKEN, TENANT_DATA.
//
//	log := tenantlog.New("deploy")                         // folder + tab: "deploy"
//	log.Info("build started", "host", "build-box", "commit", sha)
//	log.Error("build failed", "step", "link", "err", err)
//
//	slog.SetDefault(slog.New(log.Handler()))              // optional: your whole program's slog → Tenant
//
// Rules for the copy: stdlib only, Go 1.21+, and NEVER import anything from
// tenant/internal here — the file must compile on its own inside your program.
// The tests beside it (not for copying) pin its format to the hub's, so an
// updated copy from this folder is always compatible.
package tenantlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Levels — the four the log book files. slog's own constants work too.
const (
	Debug = slog.LevelDebug
	Info  = slog.LevelInfo
	Warn  = slog.LevelWarn
	Error = slog.LevelError
)

// Config tunes a Logger. Every field but Source is optional.
type Config struct {
	Source   string            // folder / tab name: [a-z0-9][a-z0-9._-]{0,39} (free text is normalized)
	URL      string            // hub base URL (default: config.json dashboard.addr, else http://127.0.0.1:8770)
	Token    string            // dashboard bearer token (default: config.json dashboard.auth, else Tenant's dashboard-auth-token file)
	DataDir  string            // Tenant data dir, for the file fallback (default: Tenant's OS data dir)
	LogDir   string            // log root override (default <DataDir>/logs)
	FileOnly bool              // never try the hub — always append to the file (cron scripts, hub known down)
	Timeout  time.Duration     // per-POST timeout (default 3s)
	Retry    time.Duration     // after a connection failure, stay on the file this long before retrying the hub (default 30s)
	Defaults map[string]string // fields stamped on every line (host, version, pid, …)
	MinLevel slog.Level        // lines below this are dropped by Handler() (default Info); Log/Info/… always send
	Stderr   io.Writer         // also print each formatted line here (os.Stderr while developing)
}

// Logger files lines under one source. Safe for concurrent use.
type Logger struct {
	cfg    Config
	source string
	httpc  *http.Client

	mu     sync.Mutex
	downAt time.Time // last hub connection failure; zero = hub assumed reachable
}

// New returns a Logger for the named source with defaults discovered from
// Tenant's config.json and env. It panics on an unusable source name — a
// programming error you want at startup, not silently in production.
func New(source string) *Logger {
	l, err := NewWithConfig(Config{Source: source})
	if err != nil {
		panic("tenantlog: " + err.Error())
	}
	return l
}

// NewWithConfig is New with explicit settings; unset fields are discovered.
func NewWithConfig(cfg Config) (*Logger, error) {
	src := NormalizeSource(cfg.Source)
	if src == "" {
		return nil, fmt.Errorf("source %q has no usable characters (letters, digits, . _ -)", cfg.Source)
	}
	disc := discover()
	if cfg.URL == "" {
		cfg.URL = disc.url
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		cfg.URL = "http://" + cfg.URL
	}
	if cfg.Token == "" {
		cfg.Token = disc.token
	}
	if cfg.DataDir == "" {
		cfg.DataDir = disc.dataDir
	}
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(cfg.DataDir, "logs")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.Retry <= 0 {
		cfg.Retry = 30 * time.Second
	}
	if cfg.MinLevel == 0 {
		cfg.MinLevel = Info
	}
	return &Logger{cfg: cfg, source: src, httpc: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Source is the normalized folder / tab name.
func (l *Logger) Source() string { return l.source }

// Sub returns a Logger for another source with the same settings — one
// program, several folders (e.g. "deploy" and "deploy-audit").
func (l *Logger) Sub(source string) *Logger {
	cfg := l.cfg
	cfg.Source = source
	n, err := NewWithConfig(cfg)
	if err != nil {
		panic("tenantlog: " + err.Error())
	}
	return n
}

// Debug / Info / Warn / Error file one line. kv are alternating key, value
// pairs; values are fmt.Sprint-ed (errors print their message).
func (l *Logger) Debug(msg string, kv ...any) { _ = l.Log(Debug, msg, kvFields(kv)) }
func (l *Logger) Info(msg string, kv ...any)  { _ = l.Log(Info, msg, kvFields(kv)) }
func (l *Logger) Warn(msg string, kv ...any)  { _ = l.Log(Warn, msg, kvFields(kv)) }
func (l *Logger) Error(msg string, kv ...any) { _ = l.Log(Error, msg, kvFields(kv)) }

// Event files a line with a kind, kept in the `ev` field; a kind named
// "error" or ending in "_failed" is filed at ERROR. A hub drops `ev` from
// lines it receives over the API (only Tenant files turn events), so there
// the row shows its level; the kind stays in a line appended to the file
// directly. Known field keys get special placement: tool (shown in mono),
// agent (a chip).
func (l *Logger) Event(kind, msg string, fields map[string]string) error {
	f := map[string]string{}
	for k, v := range fields {
		f[k] = v
	}
	f["ev"] = kind
	lvl := Info
	if kind == "error" || strings.HasSuffix(kind, "_failed") {
		lvl = Error
	}
	return l.Log(lvl, msg, f)
}

// Log files one line at the given level. It never loses a line: the hub is
// tried first (unless FileOnly or recently unreachable); a connection failure
// falls back to the day's file silently; a REJECTED line (a 4xx/5xx from a
// live hub) is written to the file too and the rejection is returned.
func (l *Logger) Log(level slog.Level, msg string, fields map[string]string) error {
	e := l.entry(level, msg, fields)
	if l.cfg.Stderr != nil {
		fmt.Fprintln(l.cfg.Stderr, e.line())
	}
	if !l.cfg.FileOnly && l.hubOK() {
		err := l.post(e)
		if err == nil {
			return nil
		}
		var ce *connError
		if errors.As(err, &ce) {
			l.markDown()
		} else {
			_ = l.appendFile(e)
			return err
		}
	}
	return l.appendFile(e)
}

// Ping reports whether a hub is reachable AND accepts our token. nil means
// lines will go live; otherwise they go to the file (which is fine).
func (l *Logger) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), l.cfg.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.cfg.URL+"/healthz", nil)
	resp, err := l.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("no hub at %s: %w", l.cfg.URL, err)
	}
	resp.Body.Close()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, l.cfg.URL+"/api/logs", nil)
	l.auth(req)
	resp, err = l.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("hub at %s: %w", l.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("hub at %s is up but rejected the token (set Config.Token / TENANT_TOKEN to the hub's dashboard.auth or dashboard-auth-token)", l.cfg.URL)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("hub at %s answered HTTP %d", l.cfg.URL, resp.StatusCode)
	}
	return nil
}

// Handler adapts the Logger to log/slog, so an existing program's logging
// flows into Tenant with no call-site changes:
//
//	slog.SetDefault(slog.New(log.Handler()))
//
// Records below Config.MinLevel (default Info) are dropped; With/WithGroup
// attributes become fields (groups as dotted keys, like the hub).
func (l *Logger) Handler() slog.Handler { return &handler{l: l} }

// --- transport ---------------------------------------------------------------

type entry struct {
	at     time.Time
	source string
	level  slog.Level
	msg    string
	fields []field
}

type field struct{ key, value string }

const maxFieldBytes = 4096 // the hub's default per-value cap

func (l *Logger) entry(level slog.Level, msg string, fields map[string]string) entry {
	merged := make(map[string]string, len(fields)+len(l.cfg.Defaults))
	for k, v := range l.cfg.Defaults {
		merged[k] = v
	}
	for k, v := range fields {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e := entry{at: time.Now(), source: l.source, level: level, msg: capValue(msg, maxFieldBytes)}
	for _, k := range keys {
		e.fields = append(e.fields, field{key: cleanKey(k), value: capValue(merged[k], maxFieldBytes)})
	}
	return e
}

func (l *Logger) auth(req *http.Request) {
	if l.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+l.cfg.Token)
	}
}

func (l *Logger) post(e entry) error {
	fields := make(map[string]string, len(e.fields))
	for _, f := range e.fields {
		fields[f.key] = f.value
	}
	body, _ := json.Marshal(map[string]any{"level": LevelName(e.level), "msg": e.msg, "fields": fields})
	ctx, cancel := context.WithTimeout(context.Background(), l.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.URL+"/api/logs/"+l.source, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	l.auth(req)
	resp, err := l.httpc.Do(req)
	if err != nil {
		return &connError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hub rejected the line: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

type connError struct{ err error }

func (c *connError) Error() string { return c.err.Error() }
func (c *connError) Unwrap() error { return c.err }

func (l *Logger) hubOK() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.downAt.IsZero() || time.Since(l.downAt) > l.cfg.Retry
}

func (l *Logger) markDown() {
	l.mu.Lock()
	l.downAt = time.Now()
	l.mu.Unlock()
}

// appendFile writes the line to <LogDir>/<source>/<YYYY-MM-DD>.log — the same
// file the hub itself would write, in the same format.
func (l *Logger) appendFile(e entry) error {
	dir := filepath.Join(l.cfg.LogDir, l.source)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, e.at.Format("2006-01-02")+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(e.line() + "\n")
	return err
}

// --- the on-disk format (kept identical to tenant/internal/logs) ---------------

const timeLayout = "2006-01-02 15:04:05.000"

// line renders `<local date> <time.ms> <LEVEL> [<source>] <msg>\t<k=v …>`.
func (e entry) line() string { return e.lineFor(e.source) }

// lineFor renders the line under an explicit source (the tests use it to
// compare against the hub's own formatter).
func (e entry) lineFor(source string) string {
	var b strings.Builder
	b.WriteString(e.at.Format(timeLayout))
	b.WriteByte(' ')
	lvl := LevelName(e.level)
	b.WriteString(lvl)
	for i := len(lvl); i < 5; i++ {
		b.WriteByte(' ')
	}
	b.WriteString(" [")
	b.WriteString(source)
	b.WriteString("] ")
	b.WriteString(flattenMsg(e.msg))
	if len(e.fields) > 0 {
		b.WriteByte('\t')
		for i, f := range e.fields {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(f.key)
			b.WriteByte('=')
			b.WriteString(quoteValue(f.value))
		}
	}
	return b.String()
}

// LevelName is the fixed-width level column; custom levels bucket into four.
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

func flattenMsg(s string) string {
	if !strings.ContainsAny(s, "\n\r\t") {
		return s
	}
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
}

func quoteValue(v string) string {
	if v == "" {
		return `""`
	}
	for _, r := range v {
		if r <= ' ' || r == '"' || r == '\\' || r == '=' || r == 0x7f {
			return strconv.Quote(v)
		}
	}
	return v
}

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

// cleanKey makes a field key the hub accepts: no spaces, quotes or '=', 1-64 chars.
func cleanKey(k string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(k) {
		switch {
		case r == ' ', r == '\t', r == '\n', r == '=', r == '"':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "field"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// NormalizeSource coerces a free-form label into a valid source name the way
// the hub does: lowercase, other punctuation → '-', trimmed to 40 chars, must
// start with a letter or digit. "" when nothing usable remains.
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
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= '0' && s[0] <= '9') {
		return ""
	}
	return s
}

func kvFields(kv []any) map[string]string {
	if len(kv) == 0 {
		return nil
	}
	out := make(map[string]string, len(kv)/2+1)
	for i := 0; i < len(kv); i += 2 {
		k := fmt.Sprint(kv[i])
		if i+1 < len(kv) {
			out[k] = valueString(kv[i+1])
		} else {
			out[k] = "" // dangling key: keep it visible rather than drop it
		}
	}
	return out
}

func valueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339)
	}
	return fmt.Sprint(v)
}

// --- discovery: Tenant's own config + data dir ---------------------------------

type discovered struct{ url, token, dataDir string }

// discover reads TENANT_URL / TENANT_TOKEN / TENANT_DATA, then Tenant's
// config.json (dashboard.addr, dashboard.auth) and its dashboard-auth-token
// file, then the OS defaults Tenant itself uses. Never fails: worst case is
// loopback :8770, no token, OS data dir.
func discover() discovered {
	d := discovered{url: os.Getenv("TENANT_URL"), token: os.Getenv("TENANT_TOKEN"), dataDir: os.Getenv("TENANT_DATA")}
	if d.url == "" || d.token == "" {
		if cfgDir, err := os.UserConfigDir(); err == nil {
			cfgDir = filepath.Join(cfgDir, "tenant")
			if b, err := os.ReadFile(filepath.Join(cfgDir, "config.json")); err == nil {
				var cfg struct {
					Dashboard struct {
						Addr string `json:"addr"`
						Auth string `json:"auth"`
					} `json:"dashboard"`
				}
				if json.Unmarshal(b, &cfg) == nil {
					if d.url == "" && cfg.Dashboard.Addr != "" {
						d.url = cfg.Dashboard.Addr
					}
					if d.token == "" {
						d.token = cfg.Dashboard.Auth
					}
				}
			}
			if d.token == "" { // a default install keeps the token in its own file
				if b, err := os.ReadFile(filepath.Join(cfgDir, "dashboard-auth-token")); err == nil {
					d.token = strings.TrimSpace(string(b))
				}
			}
		}
	}
	if d.url == "" {
		d.url = "http://127.0.0.1:8770"
	}
	if d.dataDir == "" {
		d.dataDir = defaultDataDir()
	}
	return d
}

// defaultDataDir mirrors Tenant's data-dir rule: %LOCALAPPDATA%\tenant on
// Windows, ~/Library/Application Support/tenant on macOS, $XDG_DATA_HOME/tenant
// or ~/.local/share/tenant elsewhere.
func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "tenant")
		}
		return filepath.Join(home, "AppData", "Local", "tenant")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "tenant")
	default:
		if x := os.Getenv("XDG_DATA_HOME"); x != "" {
			return filepath.Join(x, "tenant")
		}
		return filepath.Join(home, ".local", "share", "tenant")
	}
}

// --- slog adapter -------------------------------------------------------------

type handler struct {
	l      *Logger
	attrs  []field
	prefix string
}

func (h *handler) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= h.l.cfg.MinLevel }

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	fields := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		fields[a.key] = a.value
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(fields, h.prefix, a)
		return true
	})
	return h.l.Log(r.Level, r.Message, fields)
}

func addAttr(fields map[string]string, prefix string, a slog.Attr) {
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
			addAttr(fields, p, g)
		}
		return
	}
	fields[prefix+a.Key] = attrString(a.Value)
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
		return valueString(v.Any())
	}
	return v.String()
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	n := *h
	n.attrs = append([]field(nil), h.attrs...)
	tmp := map[string]string{}
	for _, a := range attrs {
		addAttr(tmp, h.prefix, a)
	}
	keys := make([]string, 0, len(tmp))
	for k := range tmp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n.attrs = append(n.attrs, field{key: k, value: tmp[k]})
	}
	return &n
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.prefix = h.prefix + name + "."
	return &n
}
