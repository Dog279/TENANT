package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tenant/internal/privacy"
	"tenant/internal/version"
)

const privacyUsage = `usage:
  tenant privacy [status]                      what Tenant keeps, how long, and how to remove it
  tenant privacy retention set <class> <days|forever>
  tenant privacy prune [--yes]                 remove everything past its retention now
  tenant privacy forget [selectors] [--yes]    remove specific data (preview first)
      --session ID  --episode N  --fact N  --skill NAME  --match "text"
      --before YYYY-MM-DD  --archive-month YYYY-MM  --connector NAME  --only-agent ID
  tenant privacy sessions [--limit N]          recent sessions, to find an id to forget
  tenant privacy export --out DIR [--no-archive] [--only-agent ID]

Retention classes: episodes, facts, archive, usage, research, screenshots,
relay_inbox, eval_artifacts, and runs, route_log and logs, whose settings live
under runs.max_age_days, route.log_retention_days and logs.retention_days.`

// cmdPrivacy is `tenant privacy` (TEN-309): retention by data class, export,
// and selective deletion with a preview.
func cmdPrivacy(ctx context.Context, args []string) error {
	verb := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	switch verb {
	case "status":
		return cmdPrivacyStatus(ctx, args, os.Stdout)
	case "retention":
		return cmdPrivacyRetention(ctx, args, os.Stdout)
	case "prune":
		return cmdPrivacyPrune(ctx, args, os.Stdin, os.Stdout)
	case "forget":
		return cmdPrivacyForget(ctx, args, os.Stdin, os.Stdout)
	case "sessions":
		return cmdPrivacySessions(ctx, args, os.Stdout)
	case "export":
		return cmdPrivacyExport(ctx, args, os.Stdout)
	case "help":
		fmt.Println(privacyUsage)
		return nil
	}
	return fmt.Errorf("unknown privacy command %q\n%s", verb, privacyUsage)
}

// privacyDirs parses the common flags and resolves the directories without
// merging the model config, which privacy commands never need.
func privacyDirs(fs *flag.FlagSet, c *commonFlags, args []string) (privacy.Data, *launchConfig, error) {
	if err := fs.Parse(args); err != nil {
		return privacy.Data{}, nil, err
	}
	if fs.NArg() > 0 {
		return privacy.Data{}, nil, fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), privacyUsage)
	}
	if err := c.resolveDirs(); err != nil {
		return privacy.Data{}, nil, err
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		lc = &launchConfig{}
	}
	return privacy.Data{DataDir: c.dataDir, ConfigDir: c.cfgDir}, lc, nil
}

// retentionRow is one class in the status view and the retention setter.
type retentionRow struct {
	class   string
	days    int // effective; 0 = forever
	setting string
}

// Existing settings that already bound some classes.
const (
	classRuns     = "runs"
	classRouteLog = "route_log"
	classLogs     = "logs"
)

// retentionRows lists every class with its effective retention.
func retentionRows(lc *launchConfig) []retentionRow {
	var rows []retentionRow
	for _, c := range privacy.Classes {
		rows = append(rows, retentionRow{class: string(c), days: privacyDays(lc, c), setting: "privacy.retention_days." + string(c)})
	}
	runs := lc.Runs.MaxAgeDays
	if runs == 0 {
		runs = 90
	}
	route := 0
	if lc.Route != nil {
		route = lc.Route.LogRetentionDays
	}
	if route == 0 {
		route = 90
	}
	logDays := lc.Logs.RetentionDays
	if logDays == 0 {
		logDays = 30
	}
	for _, r := range []retentionRow{
		{classRuns, runs, "runs.max_age_days"},
		{classRouteLog, route, "route.log_retention_days"},
		{classLogs, logDays, "logs.retention_days"},
	} {
		if r.days < 0 {
			r.days = 0
		}
		rows = append(rows, r)
	}
	return rows
}

// privacyDays is a privacy class's effective retention in days, 0 = forever.
func privacyDays(lc *launchConfig, c privacy.Class) int {
	d := 0
	if lc != nil {
		d = lc.Privacy.RetentionDays[string(c)]
	}
	if c == privacy.RelayInbox {
		switch {
		case d == 0:
			return int(relayInboxRetention / (24 * time.Hour))
		case d < 0:
			return 0
		}
	}
	if d < 0 {
		return 0
	}
	return d
}

// retentionFromConfig is the retention the pruner applies.
func retentionFromConfig(lc *launchConfig) privacy.Retention {
	r := privacy.Retention{}
	for _, c := range privacy.Classes {
		if d := privacyDays(lc, c); d > 0 {
			r[c] = d
		}
	}
	return r
}

// relayInboxKeep is how long relay attachments are kept; 0 keeps them.
func relayInboxKeep(cfgDir string) time.Duration {
	lc, err := loadLaunchConfig(cfgDir)
	if err != nil {
		lc = &launchConfig{}
	}
	return time.Duration(privacyDays(lc, privacy.RelayInbox)) * 24 * time.Hour
}

func keptFor(days int) string {
	if days <= 0 {
		return "forever"
	}
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

func cmdPrivacyStatus(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("privacy status", flag.ContinueOnError)
	c := bindCommon(fs)
	d, lc, err := privacyDirs(fs, c, args)
	if err != nil {
		return err
	}
	counts := privacy.Counts(ctx, d)
	sizes := map[string]string{
		"episodes": countOf(counts, "episodes", "turns"),
		"facts":    countOf(counts, "facts", "facts"),
		"usage":    countOf(counts, "usage", "rows"),
		classRuns:  countOf(counts, "runs", "records"),
	}
	for class, dir := range map[string]string{
		"archive": "archive", "research": "research", "screenshots": "screenshots",
		"relay_inbox": "relay-inbox", "eval_artifacts": "eval-artifacts", classLogs: c.logsRoot(),
	} {
		p := dir
		if !filepath.IsAbs(p) {
			p = filepath.Join(d.DataDir, dir)
		}
		if n, bytes := dirSize(p); n > 0 {
			sizes[class] = fmt.Sprintf("%d files, %s", n, humanBytes(bytes))
		}
	}
	fmt.Fprintf(out, "What Tenant keeps (data dir %s)\n\n", d.DataDir)
	fmt.Fprintf(out, "  %-15s %-11s %-24s %s\n", "class", "kept", "now", "set with")
	for _, r := range retentionRows(lc) {
		now := sizes[r.class]
		if now == "" {
			now = "—"
		}
		fmt.Fprintf(out, "  %-15s %-11s %-24s %s\n", r.class, keptFor(r.days), now, r.setting)
	}
	if conns := storedConnectors(c.cfgDir, c.dataDir); len(conns) > 0 {
		var names []string
		for _, cn := range conns {
			names = append(names, cn.Name+" ("+strings.Join(cn.Stored, ", ")+")")
		}
		fmt.Fprintf(out, "\n  Connected services with stored credentials or data:\n    %s\n", strings.Join(names, "\n    "))
	}
	fmt.Fprintf(out, "\n  Change a period:  tenant privacy retention set <class> <days|forever>\n")
	fmt.Fprintf(out, "  Remove something: tenant privacy forget --match \"text\" (or --session, --fact, --connector …); it shows a preview first\n")
	fmt.Fprintf(out, "  Take a copy:      tenant privacy export --out DIR\n")
	return nil
}

func countOf(counts map[string]int, key, unit string) string {
	n, ok := counts[key]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// dirSize counts the regular files under dir and their total size.
func dirSize(dir string) (int, int64) {
	n, total := 0, int64(0)
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return nil
		}
		if info, ierr := e.Info(); ierr == nil {
			n++
			total += info.Size()
		}
		return nil
	})
	return n, total
}

func cmdPrivacyRetention(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 3 || args[0] != "set" {
		return fmt.Errorf("usage: tenant privacy retention set <class> <days|forever>\n%s", privacyUsage)
	}
	class, value := args[1], strings.ToLower(args[2])
	fs := flag.NewFlagSet("privacy retention", flag.ContinueOnError)
	c := bindCommon(fs)
	if _, _, err := privacyDirs(fs, c, args[3:]); err != nil {
		return err
	}
	days := -1
	if value != "forever" && value != "never" {
		n, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil || n <= 0 || n > 36500 {
			return fmt.Errorf("retention must be a number of days (1–36500) or \"forever\", not %q", args[2])
		}
		days = n
	}
	var set func(*launchConfig)
	switch class {
	case classRuns:
		set = func(lc *launchConfig) { lc.Runs.MaxAgeDays = days }
	case classRouteLog:
		// Saving validates the bound (3650 days) with the route config.
		set = func(lc *launchConfig) {
			if lc.Route == nil {
				lc.Route = &routeConfig{}
			}
			lc.Route.LogRetentionDays = days
		}
	case classLogs:
		set = func(lc *launchConfig) { lc.Logs.RetentionDays = days }
	default:
		known := false
		for _, pc := range privacy.Classes {
			if string(pc) == class {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("unknown class %q\n%s", class, privacyUsage)
		}
		set = func(lc *launchConfig) {
			if lc.Privacy.RetentionDays == nil {
				lc.Privacy.RetentionDays = map[string]int{}
			}
			switch {
			case days > 0:
				lc.Privacy.RetentionDays[class] = days
			case class == string(privacy.RelayInbox):
				lc.Privacy.RetentionDays[class] = -1 // its default is 30 days
			default:
				delete(lc.Privacy.RetentionDays, class)
			}
		}
	}
	if err := persistLaunchConfig(c.cfgDir, nil, set); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: kept %s. A running Tenant applies it at its next daily prune; `tenant privacy prune` applies it now.\n", class, keptFor(max(days, 0)))
	return nil
}

// confirm asks on in unless yes is set; a non-interactive run needs --yes.
func confirm(in io.Reader, out io.Writer, yes bool, question string) error {
	if yes {
		return nil
	}
	fmt.Fprintf(out, "%s [y/N] ", question)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return errors.New("cancelled; nothing was changed")
}

func printPlan(out io.Writer, items []privacy.Item) {
	for _, it := range items {
		fmt.Fprintf(out, "  %-7s %6d  %s\n", it.Action, it.Count, it.What)
		for _, s := range it.Samples {
			fmt.Fprintf(out, "                   %s\n", s)
		}
	}
}

func cmdPrivacyPrune(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("privacy prune", flag.ContinueOnError)
	c := bindCommon(fs)
	yes := fs.Bool("yes", false, "apply without asking")
	d, lc, err := privacyDirs(fs, c, args)
	if err != nil {
		return err
	}
	r := retentionFromConfig(lc)
	plan, err := privacy.Prune(ctx, d, r)
	if err != nil {
		return err
	}
	if plan.Empty() {
		fmt.Fprintln(out, "Nothing is past its retention period.")
		return nil
	}
	fmt.Fprintln(out, "Past its retention period:")
	printPlan(out, plan.Items)
	fmt.Fprintln(out, "  Facts distilled from pruned turns stay, and record that their source was removed.")
	if err := confirm(in, out, *yes, "Delete these for good?"); err != nil {
		return err
	}
	res, err := plan.Apply(ctx)
	if err != nil {
		return err
	}
	printResult(out, res)
	return nil
}

func printResult(out io.Writer, res *privacy.Result) {
	if len(res.Items) == 0 {
		fmt.Fprintln(out, "Done; nothing needed changing by the time it ran.")
	} else {
		fmt.Fprintln(out, "Done:")
		printPlan(out, res.Items)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
}

// int64List is a repeatable integer flag.
type int64List []int64

func (l *int64List) String() string { return fmt.Sprint(*l) }
func (l *int64List) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(part), "#"), 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not an id", part)
		}
		*l = append(*l, n)
	}
	return nil
}

// parseDay accepts YYYY-MM-DD (local midnight) or RFC 3339.
func parseDay(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

func cmdPrivacyForget(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("privacy forget", flag.ContinueOnError)
	c := bindCommon(fs)
	var sessions, skillNames, months, conns stringList
	var episodes, facts int64List
	fs.Var(&sessions, "session", "a session id (see `tenant privacy sessions`); repeatable")
	fs.Var(&episodes, "episode", "an episode id; repeatable or comma-separated")
	fs.Var(&facts, "fact", "a fact id; repeatable or comma-separated")
	fs.Var(&skillNames, "skill", "a skill name; repeatable")
	match := fs.String("match", "", "text to forget wherever it appears (ASCII letters match in any case)")
	before := fs.String("before", "", "everything recorded before this day (YYYY-MM-DD) or time (RFC 3339)")
	fs.Var(&months, "archive-month", "an archive month (YYYY-MM) to remove; repeatable")
	fs.Var(&conns, "connector", "a connected service whose stored credentials and data to remove; repeatable")
	agent := fs.String("only-agent", "", "limit the forget to one agent's data")
	yes := fs.Bool("yes", false, "delete without asking")
	d, _, err := privacyDirs(fs, c, args)
	if err != nil {
		return err
	}
	sel := privacy.Selector{Sessions: sessions, Episodes: episodes, Facts: facts, Skills: skillNames,
		Match: *match, ArchiveMonths: months, Agent: *agent}
	if *before != "" {
		t, err := parseDay(*before)
		if err != nil {
			return fmt.Errorf("--before: %q is neither YYYY-MM-DD nor an RFC 3339 time", *before)
		}
		sel.Before = t
	}
	var connPlan []connectorPlan
	for _, name := range conns {
		cp, err := planConnector(c.cfgDir, c.dataDir, name)
		if err != nil {
			return err
		}
		connPlan = append(connPlan, cp)
	}
	if sel.Empty() && len(connPlan) == 0 {
		return fmt.Errorf("say what to forget\n%s", privacyUsage)
	}
	var plan *privacy.Plan
	if !sel.Empty() {
		if plan, err = privacy.Forget(ctx, d, sel); err != nil {
			return err
		}
	}
	nothing := (plan == nil || plan.Empty()) && connectorsEmpty(connPlan)
	if nothing {
		fmt.Fprintln(out, "Nothing matches; nothing to forget.")
		return nil
	}
	fmt.Fprintln(out, "This will permanently remove:")
	if plan != nil {
		printPlan(out, plan.Items)
	}
	for _, cp := range connPlan {
		cp.print(out)
	}
	if plan != nil {
		for _, n := range plan.Notes {
			fmt.Fprintf(out, "  note: %s\n", n)
		}
	}
	if err := confirm(in, out, *yes, "Delete these for good? This cannot be undone."); err != nil {
		return err
	}
	if plan != nil {
		res, err := plan.Apply(ctx)
		if err != nil {
			return err
		}
		printResult(out, res)
	}
	for _, cp := range connPlan {
		if err := cp.apply(c.cfgDir); err != nil {
			return err
		}
		fmt.Fprintf(out, "  %s: stored credentials and data removed; set it up again to reconnect\n", cp.name)
	}
	return nil
}

func cmdPrivacySessions(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("privacy sessions", flag.ContinueOnError)
	c := bindCommon(fs)
	limit := fs.Int("limit", 20, "how many sessions to list")
	d, _, err := privacyDirs(fs, c, args)
	if err != nil {
		return err
	}
	ss, err := privacy.Sessions(ctx, d, *limit)
	if err != nil {
		return err
	}
	if len(ss) == 0 {
		fmt.Fprintln(out, "No sessions yet.")
		return nil
	}
	for _, s := range ss {
		fmt.Fprintf(out, "%s  %s → %s  %3d turn(s)  %-10s %q\n", s.ID, s.First.Format("2006-01-02 15:04"), s.Last.Format("01-02 15:04"), s.Turns, s.Agent, s.Opening)
	}
	fmt.Fprintln(out, "\nForget one: tenant privacy forget --session <id>")
	return nil
}

func cmdPrivacyExport(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("privacy export", flag.ContinueOnError)
	c := bindCommon(fs)
	dir := fs.String("out", "", "directory to write (must not exist yet, or be empty)")
	noArchive := fs.Bool("no-archive", false, "leave out the raw session events")
	agent := fs.String("only-agent", "", "export one agent's data only")
	d, _, err := privacyDirs(fs, c, args)
	if err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("--out DIR is required\n%s", privacyUsage)
	}
	res, err := privacy.Export(ctx, d, privacy.ExportOptions{
		Out: expandPath(*dir), NoArchive: *noArchive, Agent: *agent,
		Connectors: storedConnectors(c.cfgDir, c.dataDir), TenantVersion: version.Version,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Exported %d files (%s) to %s\n", res.Files, humanBytes(res.Bytes), res.Dir)
	for _, f := range res.RowFiles() {
		fmt.Fprintf(out, "  %-28s %d rows\n", f, res.Rows[f])
	}
	fmt.Fprintln(out, "  README.md says what each file holds. The export is plaintext: keep it private.")
	return nil
}

// startRetention prunes by the configured retention in a live process: once
// a few minutes after start, then daily. Classes without a retention period
// are never touched.
func startRetention(ctx context.Context, c *commonFlags, log *slog.Logger) {
	go func() {
		t := time.NewTimer(3 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			runRetention(ctx, c, log)
			t.Reset(24 * time.Hour)
		}
	}()
}

func runRetention(ctx context.Context, c *commonFlags, log *slog.Logger) {
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		return
	}
	r := retentionFromConfig(lc)
	if len(r) == 0 {
		return
	}
	plan, err := privacy.Prune(ctx, privacy.Data{DataDir: c.dataDir, ConfigDir: c.cfgDir}, r)
	if err != nil {
		log.Warn("privacy: retention prune failed", "err", err)
		return
	}
	if plan.Empty() {
		return
	}
	res, err := plan.Apply(ctx)
	if err != nil {
		log.Warn("privacy: retention prune failed", "err", err)
		return
	}
	var parts []string
	for _, it := range res.Items {
		parts = append(parts, fmt.Sprintf("%s %d %s", it.Action, it.Count, it.What))
	}
	sort.Strings(parts)
	log.Info("privacy: retention pruned", "changes", strings.Join(parts, "; "))
	for _, w := range res.Warnings {
		log.Warn("privacy: " + w)
	}
}
