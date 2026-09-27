package main

// logsctl.go: `tenant logs` — the log book from the command line, the
// journalctl and wevtutil of Tenant (docs/design/logging-v2.md §9.4).
//
// Every command is a client of the log book's API (internal/logs/logapi).
// When this account can open logbook.db, the API's own handlers run in this
// process over it; otherwise, or with --url, the requests go to the running
// dashboard: a service that runs as another account (the Windows server runs
// serve as SYSTEM) or on another machine. One client over one API, so the
// two paths have the same features.
//
// boot:current in a filter names the run of the process that answers: over
// the dashboard, serve's (or the TUI's) own; read here, the running serve's
// (else the running TUI's, else the last of them to run), from the boot
// records they keep (bootrecord.go), never this command's own.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// logsOpts are tenant logs' flags; each subcommand reads its own.
type logsOpts struct {
	format, url, token   string
	follow, reverse      bool
	limit                int
	out                  string
	sensitive, audit     bool
	since, until, bundle string
	bucket               string
	maxSize, maxAge      string
	onFull, minLevel     string
	enabled              string
	reset                bool
	export, debugFor     string
	id                   uint64
}

func cmdLogs(ctx context.Context, args []string) error { return runLogs(ctx, args, os.Stdout) }

// runLogs is tenant logs, writing to w.
func runLogs(ctx context.Context, args []string, w io.Writer) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("logs "+sub, flag.ContinueOnError)
	var o logsOpts
	fs.StringVar(&o.format, "format", "text", "output: text, json or csv")
	fs.StringVar(&o.url, "url", "", "go through this dashboard (default: read logbook.db when this account can open it, else config.json's dashboard)")
	fs.StringVar(&o.token, "token", "", "dashboard auth bearer token (default: config.json dashboard.auth, else the dashboard-auth-token file)")
	switch sub {
	case "query":
		fs.BoolVar(&o.follow, "follow", false, "keep printing new events as they are written")
		fs.IntVar(&o.limit, "limit", 100, "how many events: the newest")
		fs.BoolVar(&o.reverse, "reverse", false, "newest first")
	case "stats":
		fs.StringVar(&o.bucket, "bucket", "", "count per span of this length (15m, 1h, 1d); default all time")
	case "export":
		fs.StringVar(&o.out, "out", "", "the file to write (required)")
		fs.BoolVar(&o.sensitive, "include-sensitive", false, "include sensitive values (the export is recorded either way)")
		fs.BoolVar(&o.audit, "audit", false, "the Security log's audit export bundle, verifiable offline with the key")
		fs.StringVar(&o.since, "since", "", "--audit: from this time or duration back (7d)")
		fs.StringVar(&o.until, "until", "", "--audit: up to this time")
	case "verify":
		fs.StringVar(&o.bundle, "bundle", "", "check an audit export bundle with this install's key instead")
	case "config":
		fs.StringVar(&o.maxSize, "max-size", "", "its size limit: 64MB, 1GB; 0 for none")
		fs.StringVar(&o.maxAge, "max-age", "", "how long its events are kept: 30d, 12h; 0 for ever")
		fs.StringVar(&o.onFull, "on-full", "", "when it is full: overwrite, archive or keep")
		fs.StringVar(&o.minLevel, "min-level", "", "the least severe level written: critical, error, warning, info, verbose or all")
		fs.StringVar(&o.enabled, "enabled", "", "true or false: whether it is written at all")
		fs.BoolVar(&o.reset, "reset", false, "go back to config.json's policy")
	case "clear":
		fs.StringVar(&o.export, "export", "", "export the log to this file (NDJSON) first")
	case "debug":
		fs.StringVar(&o.debugFor, "for", "30m", "how long, at most 24h; 0 turns it off")
	case "catalog":
		fs.Uint64Var(&o.id, "id", 0, "one event ID")
	case "list", "show", "trace", "components", "recover":
	default:
		return fmt.Errorf("logs: no subcommand %q; `tenant help logs` lists them", sub)
	}
	c := bindCommon(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if o.format != "text" && o.format != "json" && o.format != "csv" {
		return fmt.Errorf("logs: --format text, json or csv")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	if sub == "verify" && o.bundle != "" { // offline, with this install's key: no log book needed
		return logsVerifyBundle(c, o.bundle, w)
	}
	lb, err := openLogbook(c, o.url, o.token)
	if err != nil {
		return err
	}
	defer lb.close()
	out := logsOut{w: w, format: o.format, loc: time.Local}
	arg := func(what string) (string, error) {
		if len(pos) == 0 {
			return "", fmt.Errorf("logs %s: name the %s", sub, what)
		}
		return pos[0], nil
	}
	switch sub {
	case "list":
		var ls logapi.Logs
		if err := lb.do(ctx, http.MethodGet, "/api/logbook/logs", nil, &ls); err != nil {
			return err
		}
		if o.format == "text" {
			fmt.Fprintln(w, "log book: "+lb.where())
		}
		return out.logs(ls.Logs)
	case "query":
		return logsQuery(ctx, lb, out, strings.Join(pos, " "), o.limit, o.reverse, o.follow)
	case "show":
		seq, err := arg("event's seq")
		if err != nil {
			return err
		}
		var d logapi.EventDetail
		if err := lb.do(ctx, http.MethodGet, "/api/events/"+url.PathEscape(seq), nil, &d); err != nil {
			return err
		}
		return out.detail(d)
	case "trace":
		id, err := arg("trace or activity ID")
		if err != nil {
			return err
		}
		var tl logapi.Timeline
		if err := lb.do(ctx, http.MethodGet, "/api/events/timeline/"+url.PathEscape(id), nil, &tl); err != nil {
			return err
		}
		return out.timeline(tl)
	case "stats":
		q := url.Values{"q": {strings.Join(pos, " ")}, "bucket": {o.bucket}}
		var st logapi.Stats
		var top logapi.Top
		if err := lb.do(ctx, http.MethodGet, "/api/events/stats?"+q.Encode(), nil, &st); err != nil {
			return err
		}
		if err := lb.do(ctx, http.MethodGet, "/api/events/top?"+q.Encode(), nil, &top); err != nil {
			return err
		}
		return out.stats(st, top)
	case "export":
		format := o.format
		if !flagSet(fs, "format") {
			format = logapi.NDJSON
		}
		return logsExport(ctx, lb, o, strings.Join(pos, " "), format, w)
	case "verify":
		var v logapi.Verify
		if err := lb.do(ctx, http.MethodGet, "/api/logbook/verify?"+url.Values{"log": pos}.Encode(), nil, &v); err != nil {
			return err
		}
		if err := out.verify(v); err != nil {
			return err
		}
		if !v.OK {
			return errors.New("logs verify: the check failed")
		}
		return nil
	case "config":
		name, err := arg("log")
		if err != nil {
			return err
		}
		var ch logapi.PolicyChange
		if ch, err = policyChange(o); err != nil {
			return err
		}
		var l logapi.Log
		if ch == (logapi.PolicyChange{}) {
			err = lb.do(ctx, http.MethodGet, "/api/logbook/logs/"+url.PathEscape(name), nil, &l)
		} else {
			err = lb.do(ctx, http.MethodPatch, "/api/logbook/logs/"+url.PathEscape(name), ch, &l)
		}
		if err != nil {
			return err
		}
		return out.logs([]logapi.Log{l})
	case "clear":
		name, err := arg("log")
		if err != nil {
			return err
		}
		if !logs.ValidSource(name) {
			return fmt.Errorf("logs clear: %q is not a log name", name)
		}
		if o.export != "" {
			if err := logsExport(ctx, lb, logsOpts{out: o.export}, "log:"+name, logapi.NDJSON, w); err != nil {
				return err
			}
		}
		var cl logapi.Cleared
		if err := lb.do(ctx, http.MethodPost, "/api/logbook/logs/"+url.PathEscape(name)+"/clear", nil, &cl); err != nil {
			return err
		}
		fmt.Fprintf(w, "cleared %d events from %s\n", cl.Cleared, cl.Log)
		return nil
	case "debug":
		name, err := arg("provider or log")
		if err != nil {
			return err
		}
		var l logapi.Log
		if err := lb.do(ctx, http.MethodPatch, "/api/logbook/logs/"+url.PathEscape(name), logapi.PolicyChange{DebugFor: &o.debugFor}, &l); err != nil {
			return err
		}
		if l.DebugUntil == nil {
			fmt.Fprintf(w, "%s: debug off\n", l.Name)
		} else {
			fmt.Fprintf(w, "%s: its verbose events are written until %s\n", l.Name, l.DebugUntil.In(time.Local).Format("15:04:05"))
		}
		return nil
	case "catalog":
		path := "/api/logbook/catalog"
		if len(pos) > 0 {
			path += "/" + url.PathEscape(pos[0])
		}
		if o.id != 0 {
			path += "?id=" + strconv.FormatUint(o.id, 10)
		}
		var cat logapi.Catalog
		if err := lb.do(ctx, http.MethodGet, path, nil, &cat); err != nil {
			return err
		}
		return out.catalog(cat)
	case "recover":
		var body struct {
			File string `json:"file,omitempty"`
		}
		if len(pos) > 0 {
			body.File = filepath.Base(pos[0])
		}
		var r logapi.Recovered
		if err := lb.do(ctx, http.MethodPost, "/api/logbook/recover", body, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "recovered %d events from %s\n", r.Recovered, r.File)
		if r.Problems != "" {
			fmt.Fprintln(w, "could not read: "+r.Problems)
		}
		return nil
	case "components":
		var cs logapi.Components
		if err := lb.do(ctx, http.MethodGet, "/api/logbook/components", nil, &cs); err != nil {
			return err
		}
		return out.components(cs.Components)
	}
	return nil
}

func flagSet(fs *flag.FlagSet, name string) (set bool) {
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// logsQuery prints the newest limit events matching q, oldest first (newest
// first with reverse), then with follow every new one as it is written,
// resuming after the last one printed whenever the stream drops.
func logsQuery(ctx context.Context, lb *logbook, out logsOut, q string, limit int, reverse, follow bool) error {
	if follow && reverse {
		return errors.New("logs query: --follow prints oldest first; drop --reverse")
	}
	var page logapi.EventsPage
	v := url.Values{"q": {q}, "limit": {strconv.Itoa(limit)}, "reverse": {"1"}}
	if err := lb.do(ctx, http.MethodGet, "/api/events?"+v.Encode(), nil, &page); err != nil {
		return err
	}
	evs := page.Events
	if !reverse {
		slices.Reverse(evs)
	}
	ew, err := out.events()
	if err != nil {
		return err
	}
	for _, e := range evs {
		if err := ew.Write(e); err != nil {
			return err
		}
	}
	if err := ew.Flush(); err != nil || !follow {
		return err
	}
	last, err := store.ParseCursor(page.Next)
	if err != nil {
		return err
	}
	if len(evs) > 0 {
		last.Seq = evs[len(evs)-1].Seq
	}
	cursor := last.String()
	for {
		err := lb.stream(ctx, "/api/events/stream?"+url.Values{"q": {q}}.Encode(), cursor, func(m sseMessage) error {
			if m.event == "restart" {
				slog.Warn("logs: the log book was restored or rebuilt; starting over")
				return nil
			}
			var e event.Event
			if err := json.Unmarshal([]byte(m.data), &e); err != nil {
				return err
			}
			if err := ew.Write(e); err != nil {
				return err
			}
			cursor = m.id
			return ew.Flush()
		})
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("logs: the live tail dropped; resuming", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// logsExport writes every event matching q to o.out, after a header record,
// or with o.audit the Security log's audit bundle. The server records the
// export. The file is owner-only; a failed export leaves none.
func logsExport(ctx context.Context, lb *logbook, o logsOpts, q, format string, w io.Writer) (err error) {
	if o.out == "" {
		return errors.New("logs export: --out FILE required")
	}
	path := "/api/events/export?" + url.Values{"q": {q}, "format": {format}, "sensitive": {strconv.FormatBool(o.sensitive)}}.Encode()
	if o.audit {
		path = "/api/logbook/audit?" + url.Values{"since": {o.since}, "until": {o.until}}.Encode()
	}
	resp, err := lb.send(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := fsperm.OpenFile(o.out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(o.out)
		}
	}()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return fmt.Errorf("logs export: %w", err)
	}
	fmt.Fprintf(w, "exported to %s (%d bytes)\n", o.out, n)
	return nil
}

// logsVerifyBundle checks an audit export bundle with this install's key.
func logsVerifyBundle(c *commonFlags, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr, v, err := verifySecurityBundle(c, f)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "bundle from %s (store %s), exported %s: %d records up to %d\n", hdr.Host, hdr.StoreID, hdr.Time.Local().Format(time.RFC3339), v.Records, v.Head.Rec)
	for _, n := range v.Notes {
		fmt.Fprintln(w, "  note: "+n)
	}
	if v.Break != nil {
		return fmt.Errorf("logs verify: the bundle's chain breaks at %v", v.Break)
	}
	fmt.Fprintln(w, "the chain verifies")
	return nil
}

// policyChange is config's flags as a PATCH body.
func policyChange(o logsOpts) (logapi.PolicyChange, error) {
	var ch logapi.PolicyChange
	if o.maxSize != "" {
		n, err := parseSize(o.maxSize)
		if err != nil {
			return ch, err
		}
		ch.MaxBytes = &n
	}
	if o.maxAge != "" {
		d, err := logapi.Duration(o.maxAge)
		if o.maxAge == "0" {
			d, err = 0, nil
		}
		if err != nil || d < 0 {
			return ch, fmt.Errorf("--max-age: a duration such as 30d or 12h, or 0")
		}
		s := int64(d / time.Second)
		ch.MaxAgeS = &s
	}
	if o.onFull != "" {
		ch.OnFull = &o.onFull
	}
	if o.minLevel != "" {
		ch.MinLevel = &o.minLevel
	}
	if o.enabled != "" {
		b, err := strconv.ParseBool(o.enabled)
		if err != nil {
			return ch, fmt.Errorf("--enabled: true or false")
		}
		ch.Enabled = &b
	}
	ch.Reset = o.reset
	return ch, nil
}

// parseSize reads a size: bytes, or a number with KB, MB or GB (powers of
// 1024, as config.json's max_mb).
func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		n      int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if n, ok := strings.CutSuffix(t, u.suffix); ok {
			t, mult = n, u.n
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("--max-size: a size such as 64MB or 1GB, or 0")
	}
	return int64(f * float64(mult)), nil
}

// --- the client -----------------------------------------------------------

// logbook reaches the log book's API: in this process over logbook.db, or
// through a dashboard.
type logbook struct {
	base, token string
	http        *http.Client
	local       string // logbook.db's path when the API runs in this process
	why         string // why logbook.db isn't read here
	close       func()
}

// openLogbook reads logbook.db in this process when this account can open
// it, unless url names a dashboard to go through.
func openLogbook(c *commonFlags, rawURL, token string) (*logbook, error) {
	var why string
	if rawURL == "" {
		lb, err := openLocalLogbook(c)
		if err == nil {
			return lb, nil
		}
		why = "logbook.db can't be read here: " + err.Error()
		if errors.Is(err, fs.ErrNotExist) {
			why = "there is no logbook.db at " + filepath.Join(c.logsRoot(), "logbook.db")
		}
	}
	if token == "" {
		var err error
		if token, err = dashboardClientToken(c.cfgDir, c.lc, false); err != nil {
			return nil, fmt.Errorf("logs: %w", err)
		}
	}
	return &logbook{base: attachBaseURL(strings.TrimSpace(rawURL), c.lc), token: token, http: &http.Client{}, why: why, close: func() {}}, nil
}

// openLocalLogbook runs the API in this process over the logbook.db this
// account can open, with a hub of its own for the events the API writes
// (an export's record, a changed policy).
func openLocalLogbook(c *commonFlags) (*logbook, error) {
	root := c.logsRoot()
	path := filepath.Join(root, "logbook.db")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	boot, started := servingBoot(path, root)
	key, _ := readChainKey(c.cfgDir) // never created here; without it, no security record is written
	st, err := store.Open(path, store.Options{BootID: boot, ChainKey: key})
	if err != nil {
		return nil, err
	}
	h, err := logs.Open(root, c.logsOptions())
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	h.AttachStore(st)
	api := &logapi.API{Hub: func() *logs.Hub { return h }, BootStart: started, Security: securityLogAPI(c), Runs: localRuns(c),
		Components: runningComponents(c), Surface: surfaceCLI}
	return &logbook{base: "http://logbook", http: &http.Client{Transport: inProcess{api.Handler()}}, local: path,
		close: func() { _ = h.Close() }}, nil
}

// servingBoot is the boot boot:current names when tenant logs reads
// logbook.db itself, and when it started: the running serve's, else the
// running TUI's. Their rows are in the boots table, and a boot lock that
// can be taken belongs to a process that has exited (internal/logs/boots.go).
func servingBoot(path, root string) (string, time.Time) {
	st, err := store.Open(path, store.Options{})
	if err != nil {
		return "", time.Time{}
	}
	defer st.Close()
	boots, _ := st.Unfinished(context.Background(), runtrace.BootID())
	var best store.Boot
	for _, b := range boots {
		if !bootIDRe.MatchString(b.ID) { // it names a file
			continue
		}
		lock, err := datalock.ExclusiveFile(filepath.Join(root, "boots", b.ID+".lock"))
		if err == nil { // its process is gone
			_ = lock.Release()
			continue
		}
		if best.ID == "" || b.Role == "serve" && best.Role != "serve" || b.Role == best.Role && b.Started.After(best.Started) {
			best = b
		}
	}
	return best.ID, best.Started
}

var bootIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// runningComponents lists what the running serve (or TUI) runs, through its
// dashboard: only that process knows.
func runningComponents(c *commonFlags) func(ctx context.Context) ([]logapi.Component, error) {
	return func(ctx context.Context) ([]logapi.Component, error) {
		tok, err := dashboardClientToken(c.cfgDir, c.lc, false)
		if err != nil {
			return nil, err
		}
		lb := &logbook{base: attachBaseURL("", c.lc), token: tok, http: &http.Client{}}
		var cs logapi.Components
		if err := lb.do(ctx, http.MethodGet, "/api/logbook/components", nil, &cs); err != nil {
			return nil, fmt.Errorf("only the running serve or TUI knows what it runs, and its dashboard doesn't answer (%w)", err)
		}
		return cs.Components, nil
	}
}

// localRuns finds a trace's run record in runs.db, opened for the lookup.
func localRuns(c *commonFlags) logapi.RunLookup {
	path := filepath.Join(c.dataDir, "runs.db")
	return func(ctx context.Context, id string) (runtrace.Run, error) {
		if _, err := os.Stat(path); err != nil {
			return runtrace.Run{}, err
		}
		rs, err := runtrace.Open(path, effectiveRunTraceConfig(c.lc))
		if err != nil {
			return runtrace.Run{}, err
		}
		defer rs.Close()
		return rs.Get(ctx, id)
	}
}

func (lb *logbook) where() string {
	if lb.local != "" {
		return lb.local
	}
	return "through " + lb.base
}

// send makes one request, a JSON body when body isn't nil, and returns a 2xx
// response; any other status is an error carrying the API's message.
func (lb *logbook) send(ctx context.Context, method, path string, body any, hdr http.Header) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, lb.base+path, r)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if lb.token != "" {
		req.Header.Set("Authorization", "Bearer "+lb.token)
	}
	resp, err := lb.http.Do(req)
	if err != nil {
		if lb.why != "" {
			return nil, fmt.Errorf("logs: no dashboard answers at %s (%v), and %s", lb.base, err, lb.why)
		}
		return nil, fmt.Errorf("logs: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		var e struct{ Error string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		if resp.StatusCode == http.StatusUnauthorized {
			e.Error += " (--token, or the dashboard's dashboard.auth / dashboard-auth-token)"
		}
		return nil, fmt.Errorf("logs: %s (HTTP %d)", e.Error, resp.StatusCode)
	}
	return resp, nil
}

// do is send for a JSON answer, decoded into out.
func (lb *logbook) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	resp, err := lb.send(ctx, method, path, body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

type sseMessage struct{ id, event, data string }

// stream reads a server-sent event stream from after last, handing fn each
// message, until the stream ends, ctx is done, or fn fails.
func (lb *logbook) stream(ctx context.Context, path, last string, fn func(sseMessage) error) error {
	resp, err := lb.send(ctx, http.MethodGet, path, nil, http.Header{"Last-Event-Id": {last}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var m sseMessage
	for sc.Scan() {
		switch line := sc.Text(); {
		case line == "":
			if m.data != "" || m.event != "" {
				if err := fn(m); err != nil {
					return err
				}
			}
			m = sseMessage{}
		case strings.HasPrefix(line, "id: "):
			m.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			m.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			m.data = line[6:]
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

// inProcess serves requests with a handler in this process. A pipe carries
// the response body, so a stream arrives as it is written; closing the body
// ends the handler's request.
type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	pr, pw := io.Pipe()
	rw := &pipeResponse{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		defer pw.Close()
		defer rw.WriteHeader(http.StatusOK) // a handler that wrote nothing
		t.h.ServeHTTP(rw, r.WithContext(ctx))
	}()
	<-rw.ready
	return &http.Response{Status: strconv.Itoa(rw.code) + " " + http.StatusText(rw.code), StatusCode: rw.code,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: rw.sent, Body: pipeBody{pr, cancel}, Request: r}, nil
}

type pipeResponse struct {
	header, sent http.Header
	pw           *io.PipeWriter
	code         int
	once         sync.Once
	ready        chan struct{}
}

func (w *pipeResponse) Header() http.Header { return w.header }

func (w *pipeResponse) WriteHeader(code int) {
	w.once.Do(func() {
		w.code, w.sent = code, w.header.Clone()
		close(w.ready)
	})
}

func (w *pipeResponse) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(p)
}

func (w *pipeResponse) Flush() {}

type pipeBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (c pipeBody) Close() error {
	c.cancel()
	return c.PipeReader.Close()
}

// --- output -------------------------------------------------------------

// logsOut writes the answers in one format: text for people, json and csv
// for programs. Times in text are local (loc); in json and csv, UTC.
type logsOut struct {
	w      io.Writer
	format string
	loc    *time.Location
}

func (o logsOut) json(v any) error {
	enc := json.NewEncoder(o.w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (o logsOut) csv(rows [][]string) error {
	cw := csv.NewWriter(o.w)
	_ = cw.WriteAll(rows)
	return cw.Error()
}

func (o logsOut) table(rows [][]string) error {
	tw := tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// events writes events: the text mirror's line after the seq, NDJSON, or
// CSV (logapi's writer, as exports are written).
func (o logsOut) events() (*logapi.Writer, error) {
	f := o.format
	if f == "json" {
		f = logapi.NDJSON
	}
	return logapi.NewWriter(o.w, f, o.loc)
}

func (o logsOut) when(t *time.Time) string {
	if t == nil || t.IsZero() {
		if o.format == "text" {
			return "-"
		}
		return ""
	}
	if o.format == "text" {
		return t.In(o.loc).Format("2006-01-02 15:04:05")
	}
	return t.UTC().Format(time.RFC3339)
}

func (o logsOut) logs(ls []logapi.Log) error {
	switch o.format {
	case "json":
		return o.json(logapi.Logs{Logs: ls})
	case "csv":
		rows := [][]string{{"name", "group", "events", "bytes", "max_bytes", "max_age_s", "on_full", "min_level", "enabled", "last_write", "dropped", "suppressed", "debug_until", "cleared"}}
		for _, l := range ls {
			p := l.Policy
			rows = append(rows, []string{l.Name, strings.Join(l.Group, "/"), itoa(l.Events), itoa(l.Bytes), itoa(p.MaxBytes), itoa(p.MaxAgeS),
				p.OnFull, p.MinLevel, strconv.FormatBool(p.Enabled), o.when(l.LastWrite), itoa(l.Dropped), itoa(l.Suppressed), o.when(l.DebugUntil), o.when(l.Cleared)})
		}
		return o.csv(rows)
	}
	rows := [][]string{{"LOG", "GROUP", "EVENTS", "SIZE", "LIMIT", "KEPT", "WHEN FULL", "LEVEL", "LAST WRITE", "NOTES"}}
	for _, l := range ls {
		p := l.Policy
		level := p.MinLevel
		if level == "" {
			level = "all"
		}
		var notes []string
		if !p.Enabled {
			notes = append(notes, "disabled")
		}
		if l.DebugUntil != nil {
			notes = append(notes, "debug until "+l.DebugUntil.In(o.loc).Format("15:04"))
		}
		if l.Dropped+l.Suppressed > 0 {
			notes = append(notes, fmt.Sprintf("%d dropped, %d suppressed", l.Dropped, l.Suppressed))
		}
		if l.Cleared != nil {
			notes = append(notes, "cleared "+o.when(l.Cleared))
		}
		rows = append(rows, []string{l.Name, strings.Join(l.Group, " › "), itoa(l.Events), sizeText(l.Bytes), sizeText(p.MaxBytes),
			ageText(p.MaxAgeS), p.OnFull, level, o.when(l.LastWrite), strings.Join(notes, "; ")})
	}
	return o.table(rows)
}

func (o logsOut) detail(d logapi.EventDetail) error {
	switch o.format {
	case "json":
		return o.json(d)
	case "csv":
		rows := [][]string{{"field", "value"}}
		for _, kv := range eventFields(d.Event, o) {
			rows = append(rows, []string{kv[0], kv[1]})
		}
		if d.Stats != nil {
			rows = append(rows, []string{"seen", itoa(d.Stats.Count)}, []string{"first_seen", o.when(d.Stats.First)},
				[]string{"last_seen", o.when(d.Stats.Last)}, []string{"trend_7d", trendText(d.Stats.Trend)})
		}
		if d.Help != "" {
			rows = append(rows, []string{"help", d.Help})
		}
		return o.csv(rows)
	}
	e := d.Event
	fmt.Fprintf(o.w, "Event %d · %s · %s\n", e.Seq, e.Level, eventName(e))
	for _, kv := range eventFields(e, o) {
		if kv[1] != "" && kv[0] != "seq" && kv[0] != "level" {
			fmt.Fprintf(o.w, "  %-12s %s\n", kv[0], kv[1])
		}
	}
	if d.Stats != nil {
		fmt.Fprintf(o.w, "\nSeen %d times: first %s, last %s; per day over the last 7: %s\n", d.Stats.Count, o.when(d.Stats.First), o.when(d.Stats.Last), trendText(d.Stats.Trend))
	}
	if d.Help != "" {
		fmt.Fprintf(o.w, "\n%s", d.Help)
	}
	return nil
}

// eventFields are an event's fields as name, text pairs, its data last.
func eventFields(e event.Event, o logsOut) [][2]string {
	c := e.Corr
	t := e.Time
	out := [][2]string{{"seq", strconv.FormatUint(e.Seq, 10)}, {"time", o.when(&t)}, {"level", e.Level.String()},
		{"log", e.Log}, {"provider", e.Provider}, {"id", ""}, {"task", e.Task}, {"opcode", e.Opcode},
		{"keywords", strings.Join(e.Keywords, " ")}, {"msg", e.Msg},
		{"trace", c.TraceID}, {"activity", c.ActivityID}, {"related", c.RelatedID}, {"session", c.SessionID},
		{"approval", c.ApprovalID}, {"tool_call", c.ToolCallID}, {"actor", e.Actor}, {"surface", e.Surface},
		{"pid", ""}, {"boot", e.Proc.BootID}, {"host", e.Proc.Host}, {"version", e.Proc.Version}}
	if e.ID != 0 {
		out[5][1] = strconv.FormatUint(uint64(e.ID), 10)
	}
	if e.Proc.PID != 0 {
		out[18][1] = strconv.Itoa(e.Proc.PID)
	}
	if o.format == "text" && e.Channel == event.Diagnostic {
		out = append(out, [2]string{"channel", "diagnostic"})
	}
	for _, f := range e.Data {
		out = append(out, [2]string{"data." + f.Key, f.Text()})
	}
	return out
}

func eventName(e event.Event) string {
	if e.ID == 0 {
		return e.Provider + " (uncatalogued)"
	}
	return e.Provider + "/" + strconv.FormatUint(uint64(e.ID), 10)
}

func (o logsOut) timeline(tl logapi.Timeline) error {
	if o.format == "json" {
		return o.json(tl)
	}
	if o.format == "text" {
		if r := tl.Run; r != nil {
			fmt.Fprintf(o.w, "Run %s: %s · %s · %s %s · %d ms · %d+%d tokens · %d tool calls · %d approvals\n", r.TraceID, r.Outcome, r.Source,
				r.Provider, r.Model, r.LatencyMS, r.PromptTokens, r.CompletionTokens, len(r.ToolCalls), len(r.Approvals))
		} else {
			fmt.Fprintf(o.w, "%s: %s\n", tl.ID, tl.RunNote)
		}
	}
	ew, err := o.events()
	if err != nil {
		return err
	}
	for _, e := range tl.Events {
		if err := ew.Write(e); err != nil {
			return err
		}
	}
	return ew.Flush()
}

func (o logsOut) stats(st logapi.Stats, top logapi.Top) error {
	switch o.format {
	case "json":
		return o.json(struct {
			logapi.Stats
			logapi.Top
		}{st, top})
	case "csv":
		rows := [][]string{{"kind", "start", "log", "level", "provider", "id", "n"}}
		for _, b := range st.Buckets {
			start := ""
			if b.Start != nil {
				start = o.when(b.Start)
			}
			rows = append(rows, []string{"count", start, b.Log, b.Level, "", "", itoa(b.N)})
		}
		for _, t := range top.Top {
			rows = append(rows, []string{"top", "", "", "", t.Provider, strconv.FormatUint(uint64(t.ID), 10), itoa(t.N)})
		}
		return o.csv(rows)
	}
	rows := [][]string{{"FROM", "LOG", "LEVEL", "EVENTS"}}
	for _, b := range st.Buckets {
		start := "all time"
		if b.Start != nil {
			start = o.when(b.Start)
		}
		rows = append(rows, []string{start, b.Log, b.Level, itoa(b.N)})
	}
	if err := o.table(rows); err != nil {
		return err
	}
	if len(top.Top) == 0 {
		return nil
	}
	fmt.Fprintln(o.w, "\nMost frequent event IDs:")
	rows = [][]string{{"EVENT", "TIMES"}}
	for _, t := range top.Top {
		rows = append(rows, []string{t.Provider + "/" + strconv.FormatUint(uint64(t.ID), 10), itoa(t.N)})
	}
	return o.table(rows)
}

func (o logsOut) catalog(c logapi.Catalog) error {
	switch o.format {
	case "json":
		return o.json(c)
	case "csv":
		rows := [][]string{{"provider", "id", "level", "channel", "task", "template", "log", "retired"}}
		for _, s := range c.Specs {
			rows = append(rows, []string{s.Provider, strconv.FormatUint(uint64(s.ID), 10), s.Level, s.Channel, s.Task, s.Template, s.Log, strconv.FormatBool(s.Retired)})
		}
		return o.csv(rows)
	}
	_, err := io.WriteString(o.w, c.Markdown)
	return err
}

func (o logsOut) components(cs []logapi.Component) error {
	switch o.format {
	case "json":
		return o.json(logapi.Components{Components: cs})
	case "csv":
		rows := [][]string{{"name", "kind", "state", "since", "started", "log", "error", "activity"}}
		for _, c := range cs {
			rows = append(rows, []string{c.Name, string(c.Kind), string(c.State), o.when(&c.Since), o.when(&c.Started), c.Log, c.Error, c.Activity})
		}
		return o.csv(rows)
	}
	if len(cs) == 0 {
		_, err := fmt.Fprintln(o.w, "nothing is running")
		return err
	}
	rows := [][]string{{"NAME", "KIND", "STATE", "SINCE", "LOG", "LAST ERROR"}}
	for _, c := range cs {
		rows = append(rows, []string{c.Name, string(c.Kind), string(c.State), o.when(&c.Since), c.Log, c.Error})
	}
	return o.table(rows)
}

func (o logsOut) verify(v logapi.Verify) error {
	switch o.format {
	case "json":
		return o.json(v)
	case "csv":
		rows := [][]string{{"check", "ok", "detail"}, {"integrity", strconv.FormatBool(len(v.Problems) == 0), strings.Join(v.Problems, "; ")}}
		if c := v.Chain; c != nil {
			detail := fmt.Sprintf("%d records up to %d; %d archives; %d tombstones; %d restores", c.Records, c.Head, c.Archives, c.Tombstones, c.Restores)
			for _, n := range c.Notes {
				detail += "; note: " + n
			}
			if c.Break != "" {
				detail += "; breaks at " + c.Break
			}
			rows = append(rows, []string{"chain", strconv.FormatBool(c.Break == ""), detail})
		}
		if v.Note != "" {
			rows = append(rows, []string{"note", "", v.Note})
		}
		return o.csv(rows)
	}
	if len(v.Problems) == 0 {
		fmt.Fprintln(o.w, "logbook.db: the integrity check passed")
	}
	for _, p := range v.Problems {
		fmt.Fprintln(o.w, "logbook.db: "+p)
	}
	if c := v.Chain; c != nil {
		fmt.Fprintf(o.w, "security: %d records verified up to record %d, %d archives, %d tombstones, %d restores\n", c.Records, c.Head, c.Archives, c.Tombstones, c.Restores)
		for _, n := range c.Notes {
			fmt.Fprintln(o.w, "  note: "+n)
		}
		if c.Break != "" {
			fmt.Fprintln(o.w, "security: the chain breaks at "+c.Break)
		}
	}
	if v.Note != "" {
		fmt.Fprintln(o.w, "note: "+v.Note)
	}
	return nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func sizeText(n int64) string {
	switch {
	case n == 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func ageText(s int64) string {
	switch d := time.Duration(s) * time.Second; {
	case s == 0:
		return "forever"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	default:
		return d.String()
	}
}

func trendText(t [7]int64) string {
	parts := make([]string, len(t))
	for i, n := range t {
		parts[i] = itoa(n)
	}
	return strings.Join(parts, " ")
}
