package main

// logctl.go: `tenant log` — the command-line half of the live logging API.
//
//	tenant log <source> "<message>" [key=value …] [--level warn|error]
//	tenant log --list
//
// A script, a cron job, a sidecar, or a person at a shell files a line into
// the log book under <source>; the folder (<data>/logs/<source>/) and the
// dashboard tab appear on first use. The line goes through the running hub's
// POST /api/logs/{source} when a hub is reachable (so open dashboards and TUI
// feeds see it live); otherwise it is left in the spool (<logs root>/spool/),
// which the next Tenant process to run files into the log book, tagged
// external. No rebuild, no registration.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
)

func cmdLog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	level := fs.String("level", "info", "debug|info|warn|error")
	urlFlag := fs.String("url", "", "hub dashboard URL (default: config.json dashboard address)")
	token := fs.String("token", "", "dashboard auth bearer token (default: config.json dashboard.auth, else the dashboard-auth-token file)")
	list := fs.Bool("list", false, "list the log book's sources (folders) and exit")
	direct := fs.Bool("file", false, "spool the line without trying the running hub")
	c := bindCommon(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	base := attachBaseURL(strings.TrimSpace(*urlFlag), c.lc)
	tok := *token
	if tok == "" && (*list || !*direct) {
		if tok, err = dashboardClientToken(c.cfgDir, c.lc, false); err != nil {
			return fmt.Errorf("log: %w", err)
		}
	}
	httpc := &http.Client{Timeout: 5 * time.Second}

	if *list {
		return logList(ctx, httpc, base, tok, c)
	}
	if len(pos) < 2 {
		return fmt.Errorf("usage: tenant log <source> \"<message>\" [key=value …] [--level warn|error]")
	}
	source := logs.NormalizeSource(pos[0])
	if source == "" {
		return fmt.Errorf("log: %q is not a usable source name (letters, digits, . _ -)", pos[0])
	}
	if logs.Builtin(source) { // the hub refuses these too; the file path must not differ
		return fmt.Errorf("log: %q is one of Tenant's own logs; pick another source name", source)
	}
	msg := strings.TrimSpace(pos[1])
	if msg == "" {
		return fmt.Errorf("log: message is empty")
	}
	lvl, ok := logs.ParseLevel(*level)
	if !ok {
		return fmt.Errorf("log: --level must be debug|info|warn|error")
	}
	fields := map[string]string{}
	for _, kv := range pos[2:] {
		k, v, found := strings.Cut(kv, "=")
		if !found || k == "" {
			return fmt.Errorf("log: %q is not key=value", kv)
		}
		fields[k] = v
	}

	filed := logs.ExtLog(source) // the log it lands in: outside writers file under ext.
	if !*direct {
		if err := logPost(ctx, httpc, base, tok, source, lvl, msg, fields); err == nil {
			fmt.Printf("logged to %s via %s\n", filed, base)
			return nil
		} else if !isConnErr(err) {
			return fmt.Errorf("log: hub rejected the line: %w", err)
		}
	}
	// No hub running (or --file): leave the line in the spool, which the
	// next Tenant process to run files, tagged external.
	ev := event.Event{Time: time.Now().UTC(), Log: source, Level: event.LevelFromSlog(lvl), Msg: msg}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		ev.Data = append(ev.Data, event.Field{Key: k, Value: fields[k]})
	}
	if err := logs.Spool(c.logsRoot(), ev); err != nil {
		return fmt.Errorf("log: %w", err)
	}
	fmt.Printf("spooled to %s (no hub reachable; Tenant files it from %s when it runs)\n", filed, c.logsRoot())
	return nil
}

// parseInterspersed parses flags that may appear before OR after the
// positional arguments (`tenant log deploy "started" --level warn`), which the
// stdlib flag package alone does not allow.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// Take positionals up to the next flag, then parse from that flag on.
		i := 0
		for i < len(rest) && !(strings.HasPrefix(rest[i], "-") && len(rest[i]) > 1) {
			i++
		}
		pos = append(pos, rest[:i]...)
		if i == len(rest) {
			return pos, nil
		}
		args = rest[i:]
	}
}

func logPost(ctx context.Context, httpc *http.Client, base, token, source string, lvl interface{ String() string }, msg string, fields map[string]string) error {
	body, _ := json.Marshal(map[string]any{"level": lvl.String(), "msg": msg, "fields": fields})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/logs/"+source, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return &connErr{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("401 unauthorized — wrong/missing --token (the hub's dashboard.auth or dashboard-auth-token)")
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

type connErr struct{ err error }

func (c *connErr) Error() string { return c.err.Error() }
func (c *connErr) Unwrap() error { return c.err }

func isConnErr(err error) bool {
	_, ok := err.(*connErr)
	return ok
}

// logList prints the sources: from the running hub when reachable, else from
// the folders on disk.
func logList(ctx context.Context, httpc *http.Client, base, token string, c *commonFlags) error {
	var resp struct {
		Root    string `json:"root"`
		Sources []struct {
			Name   string `json:"name"`
			Native bool   `json:"native"`
			Count  int    `json:"count"`
			LastAt string `json:"last_at"`
			Err    string `json:"err"`
		} `json:"sources"`
	}
	if err := attachGet(ctx, httpc, base+"/api/logs", token, &resp); err == nil {
		fmt.Printf("log book at %s (live from %s)\n", resp.Root, base)
		for _, s := range resp.Sources {
			who := "tenant"
			if !s.Native {
				who = "external"
			}
			line := fmt.Sprintf("  %-20s %-8s %5d retained", s.Name, who, s.Count)
			if s.LastAt != "" {
				line += "  last " + s.LastAt
			}
			if s.Err != "" {
				line += "  ⚠ " + s.Err
			}
			fmt.Println(line)
		}
		return nil
	}
	root := c.logsRoot()
	ents, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("log: no hub reachable at %s and no log root at %s", base, root)
	}
	fmt.Printf("log book at %s (hub not reachable; folders on disk)\n", root)
	for _, e := range ents {
		if e.IsDir() && logs.ValidSource(e.Name()) {
			fmt.Println("  " + e.Name())
		}
	}
	return nil
}
