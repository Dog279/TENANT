package main

// attach.go (TEN-248): `tenant attach` — a read-only terminal client onto a
// RUNNING serve hub. Where `tenant serve` runs the headless 24/7 daemon and the
// web dashboard is the rich control surface, this is the cheap "hop in to watch"
// half of the TEN-194 vision: a structured `journalctl -f` for the hub.
//
// It reads the daemon's EXISTING GET /api/activity?since=<cursor> projection
// (gap-free, cursor-based event replay) + GET /api/status — no new server-side
// control plane, no second *agent.Agent against the shared stores. v1 is
// read-only (monitoring); interactive chat/interject attach is deferred to a
// follow-up (TEN-248b) pending the dashboard-transport refactor.
//
// Auth reuses the dashboard's bearer token: --token, else an explicit
// config.json dashboard.auth, else the generated config-dir token.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// attachEvent mirrors the dashboard's restActivityEvent JSON (stable since
// TEN-194). Kept local so the client doesn't import the dashboard package.
type attachEvent struct {
	Seq    uint64 `json:"seq"`
	At     string `json:"at"`
	Kind   string `json:"kind"`
	Agent  string `json:"agent,omitempty"`
	Iter   int    `json:"iter,omitempty"`
	Tool   string `json:"tool,omitempty"`
	IsErr  bool   `json:"is_err,omitempty"`
	Text   string `json:"text,omitempty"`
	Source string `json:"source,omitempty"` // the log-book folder (turns, discord, cron, …)
	Level  string `json:"level,omitempty"`
}

type attachActivityResp struct {
	Events []attachEvent `json:"events"`
	Cursor uint64        `json:"cursor"`
	Epoch  string        `json:"epoch,omitempty"` // changes when the hub restarts; "" from an older hub
}

type attachStatus struct {
	Status           string `json:"status"`
	ToolsEnabled     int    `json:"tools_enabled"`
	ToolsTotal       int    `json:"tools_total"`
	TurnActive       bool   `json:"turn_active"`
	TurnAgeSecs      int    `json:"turn_age_secs"`
	PendingApprovals int    `json:"pending_approvals"`
}

func cmdAttach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "keep tailing new events (like `journalctl -f`); otherwise print the current backlog and exit")
	token := fs.String("token", "", "dashboard auth bearer token (default: local generated dashboard token)")
	since := fs.Uint64("since", 0, "start from this event cursor (0 = the start of the retained log)")
	interval := fs.Duration("interval", time.Second, "poll interval when --follow")
	source := fs.String("source", "", "only this log source / folder (e.g. discord, cron, turns); default all")
	level := fs.String("level", "", "only lines at or above this level: warn|error; default all")
	trace := fs.String("trace", "", "only one turn's lines: its trace ID (runs.db trace_id, the lines' trace= field)")
	c := bindCommon(fs)
	pos, err := parseInterspersed(fs, args) // `attach URL --follow` works too
	if err != nil {
		return err
	}
	if err := c.resolve(); err != nil { // loads c.lc (dashboard addr + auth)
		return err
	}

	var arg string
	if len(pos) > 0 {
		arg = strings.TrimSpace(pos[0])
	}
	base := attachBaseURL(arg, c.lc)
	tok := *token
	if tok == "" && c.lc != nil {
		tok = c.lc.Dashboard.Auth
	}
	if tok == "" {
		var err error
		tok, err = resolveDashboardAuth(c.cfgDir, "")
		if err != nil {
			return err
		}
	}

	httpc := &http.Client{Timeout: 15 * time.Second}

	// One status probe up front — also the reachability check. If the hub
	// isn't up, fail loudly here rather than silently tailing nothing.
	st, err := attachFetchStatus(ctx, httpc, base, tok)
	if err != nil {
		return fmt.Errorf("attach: cannot reach hub at %s: %w (is `tenant serve` running? check the URL/--token)", base, err)
	}
	fmt.Printf("attached to %s — status=%s, tools=%d/%d, turn_active=%v, pending_approvals=%d\n",
		base, st.Status, st.ToolsEnabled, st.ToolsTotal, st.TurnActive, st.PendingApprovals)
	if !*follow {
		fmt.Println("(snapshot — pass --follow to keep tailing)")
	}

	filter := ""
	if *source != "" {
		filter += "&source=" + *source
	}
	if *level != "" {
		filter += "&level=" + *level
	}
	if *trace != "" {
		filter += "&trace=" + url.QueryEscape(*trace)
	}
	cursor, epoch := *since, ""
	for {
		evs, next, nextEpoch, ferr := attachFetchActivityFiltered(ctx, httpc, base, tok, cursor, epoch, filter)
		if ferr != nil {
			if !*follow {
				return fmt.Errorf("attach: activity fetch failed: %w", ferr)
			}
			fmt.Fprintln(os.Stderr, "attach: fetch failed, retrying: "+ferr.Error())
		} else {
			if epoch != "" && nextEpoch != epoch {
				fmt.Fprintln(os.Stderr, "attach: the hub restarted; replaying the lines it kept")
			}
			for _, e := range evs {
				fmt.Println(formatAttachEvent(e))
			}
			cursor, epoch = next, nextEpoch
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

// attachBaseURL resolves the hub URL: an explicit arg wins (a bare host:port is
// assumed http); otherwise the local config's dashboard address (TEN-194's
// default 127.0.0.1:8770).
func attachBaseURL(arg string, lc *launchConfig) string {
	if arg == "" {
		dcfg, _ := resolveDashboardConfig(lc, false, false, "")
		arg = dcfg.Addr
	}
	if !strings.HasPrefix(arg, "http://") && !strings.HasPrefix(arg, "https://") {
		arg = "http://" + arg
	}
	return strings.TrimRight(arg, "/")
}

func attachGet(ctx context.Context, httpc *http.Client, url, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("401 unauthorized — wrong/missing --token (the hub's dashboard.auth)")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func attachFetchStatus(ctx context.Context, httpc *http.Client, base, token string) (attachStatus, error) {
	var st attachStatus
	err := attachGet(ctx, httpc, base+"/api/status", token, &st)
	return st, err
}

func attachFetchActivity(ctx context.Context, httpc *http.Client, base, token string, since uint64) ([]attachEvent, uint64, error) {
	evs, next, _, err := attachFetchActivityFiltered(ctx, httpc, base, token, since, "", "")
	return evs, next, err
}

// attachFetchActivityFiltered reads one page of the projection after the
// cursor since, which belongs to the hub epoch epoch ("" before the first
// page); filter is an optional "&source=…&level=…&trace=…" narrowing (the
// log-book folder, level floor, one turn). It returns the next cursor and its
// epoch.
func attachFetchActivityFiltered(ctx context.Context, httpc *http.Client, base, token string, since uint64, epoch, filter string) ([]attachEvent, uint64, string, error) {
	var resp attachActivityResp
	u := fmt.Sprintf("%s/api/activity?since=%d%s", base, since, filter)
	if epoch != "" {
		u += "&epoch=" + url.QueryEscape(epoch)
	}
	if err := attachGet(ctx, httpc, u, token, &resp); err != nil {
		return nil, since, epoch, err
	}
	// Never regress the cursor within one epoch (defensive against a
	// malformed/older response). A new epoch means the hub restarted and
	// numbers from 1 again, so its cursor is taken as it is.
	if resp.Cursor < since && (epoch == "" || resp.Epoch == epoch) {
		resp.Cursor = since
	}
	return resp.Events, resp.Cursor, resp.Epoch, nil
}

// formatAttachEvent renders one event as a single tail line: `HH:MM:SS KIND
// [agent] [tool] text`. The timestamp is reduced to a clock; a parse failure
// falls back to the raw RFC3339.
func formatAttachEvent(e attachEvent) string {
	ts := e.At
	if t, err := time.Parse(time.RFC3339, e.At); err == nil {
		ts = t.Local().Format("15:04:05")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-14s", ts, e.Kind)
	if e.Source != "" {
		fmt.Fprintf(&b, " %-10s", "["+e.Source+"]")
	}
	if e.Agent != "" {
		fmt.Fprintf(&b, " (%s)", e.Agent)
	}
	if e.Tool != "" {
		fmt.Fprintf(&b, " [%s]", e.Tool)
	}
	if e.IsErr {
		b.WriteString(" ✗")
	}
	if e.Text != "" {
		b.WriteString("  " + collapseWS(e.Text))
	}
	return b.String()
}

func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
