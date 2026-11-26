package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tenant/internal/plugins/osys"

	"tenant/internal/agent"
	croneng "tenant/internal/cron"
	"tenant/internal/logs"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
	"tenant/internal/runtrace"
)

// The exec auto-approver must hard-deny irreversible actions and writes into the
// config/data dirs (anti self-replication), while allowing benign ones.
func TestCronExecApprover(t *testing.T) {
	ap := cronExecApprover("/cfgdir", "/datadir")
	ctx := context.Background()
	deny := []struct{ action, detail string }{
		{"os_exec_dangerous", "rm -rf /  [DANGER: recursive force remove]"},
		{"sql_ddl", "DROP TABLE x"},
		{"web_transact", "purchase"},
		{"os_write", "OVERWRITE existing file /cfgdir/config.json"},
		{"os_write", "write /datadir/cron-history.json"},
	}
	for _, d := range deny {
		if ap(ctx, d.action, d.detail) {
			t.Errorf("approver must DENY %s %q", d.action, d.detail)
		}
	}
	allow := []struct{ action, detail string }{
		{"os_exec", "ls -la"},
		{"os_write", "write /tmp/report.txt"},
		{"gsuite_send", "email to boss"},
	}
	for _, a := range allow {
		if !ap(ctx, a.action, a.detail) {
			t.Errorf("approver must ALLOW %s %q", a.action, a.detail)
		}
	}
}

func TestScrubbedEnv(t *testing.T) {
	t.Setenv("CRON_TEST_API_KEY", "secret")
	t.Setenv("CRON_TEST_AUTH_TOKEN", "secret")
	t.Setenv("CRON_TEST_PLAIN", "ok")
	env := scrubbedEnv()
	var sawKey, sawToken, sawPlain bool
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "CRON_TEST_API_KEY="):
			sawKey = true
		case strings.HasPrefix(kv, "CRON_TEST_AUTH_TOKEN="):
			sawToken = true
		case strings.HasPrefix(kv, "CRON_TEST_PLAIN="):
			sawPlain = true
		}
	}
	if sawKey || sawToken {
		t.Error("secret-bearing vars must be scrubbed from the shell env")
	}
	if !sawPlain {
		t.Error("non-secret vars must survive")
	}
}

func TestRunCronShellBenign(t *testing.T) {
	out, err := runCronShell(context.Background(), "echo hello-cron", t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("benign shell command errored: %v", err)
	}
	if !strings.Contains(out, "hello-cron") {
		t.Errorf("expected captured output, got %q", out)
	}
}

func TestRunCronShellRefusesCatastrophic(t *testing.T) {
	// Echo-prefixed so it is INERT even if the classifier (or refusal) fails —
	// the test never risks running a destructive command.
	probe := "echo rm -rf /tmp/cron-probe-not-a-real-path"
	if dangerous, _ := osys.Classify(probe); !dangerous {
		t.Skip("classifier did not flag the probe; refusal path is covered by osys tests")
	}
	_, err := runCronShell(context.Background(), probe, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a classifier-flagged command must be refused, got err=%v", err)
	}
}

// The exec approver's deny-by-default swallowed two delivery-critical families
// because categorize maps unknown action ids to destructive: remote-MCP READS
// ("label:tool" ids the table has never heard of) and Discord delivery (prose
// ids). The 2026-08-22 trial brief lost its Jira section and had to deliver
// over the webhook fallback. The override approves exactly those; everything
// genuinely destructive stays denied.
func TestCronExecApprover_DestructiveOverrides(t *testing.T) {
	approve := cronExecApprover(t.TempDir(), t.TempDir())
	ctx := context.Background()

	allowed := []string{
		"atlassian:searchJiraIssuesUsingJql",
		// the REAL id: the label itself contains a colon
		"mcp:mcp.atlassian.com:searchJiraIssuesUsingJql",
		"mcp:mcp.atlassian.com:getJiraIssue",
		"atlassian:getConfluencePage",
		"atlassian:lookupJiraAccountId",
		"somemcp:listThings",
		discord.ActionSendMessage,
		discord.ActionReact,
	}
	for _, a := range allowed {
		if !approve(ctx, a, "detail") {
			t.Errorf("exec approver denied %q — the brief needs this unattended", a)
		}
	}

	denied := []string{
		"atlassian:createJiraIssue",
		"mcp:mcp.atlassian.com:editJiraIssue",
		"mcp:mcp.atlassian.com:createConfluencePage",
		"atlassian:editJiraIssue",
		"atlassian:transitionJiraIssue",
		"atlassian:deleteEverything",
		"os_exec_dangerous",
		"sql_ddl",
		"web_transact",
		"totally-unknown-action",
		":getNoLabel", // malformed id must not sneak through as a read
	}
	for _, a := range denied {
		if approve(ctx, a, "detail") {
			t.Errorf("exec approver allowed %q — destructive must stay denied unattended", a)
		}
	}

	// The write protection is untouched: config/data writes still refused.
	cfg := t.TempDir()
	approve2 := cronExecApprover(cfg, t.TempDir())
	if approve2(ctx, "os_write", "writing "+cfg+"/config.json") {
		t.Error("exec approver allowed a write into the config dir")
	}
}

// A scheduled run must be visible in the activity feed - tool calls, results,
// and the final - attributed to the cron agent. Cron runs were previously
// silent between the start line and the result summary: the operator could
// not see WHICH tools a job called without grepping, which is how "ok: true
// briefs that never delivered" stayed invisible for a week.
func TestCronRunner_EmitsActivityEvents(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	router, err := buildRouter(c, log)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	sl := soul.NewDefault("main")
	live := &soul.Live{}
	live.Store(sl)

	var mu sync.Mutex
	var events []agent.Event
	mux := newToolMux()
	runs, err := runtrace.Open(":memory:", runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	deps := cronAgentDeps{
		router:   router,
		soulLive: live,
		runs:     runs,
		fullReg:  mux,
		fullDisp: mux,
		log:      log,
		events: func(e agent.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
		cfgDir:  c.cfgDir,
		dataDir: c.dataDir,
	}
	runner, err := buildCronRunner(deps)
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	hub := logs.NewMemory(0)
	prev := logs.Default()
	logs.Install(hub)
	defer logs.Install(prev)
	_, trace, err := runner(context.Background(), croneng.Job{JobDef: croneng.JobDef{ID: "j1", Name: "test", Prompt: "say hi"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The trace the history entry carries is the turn's runs.db record (TEN-368).
	if _, err := runs.Get(context.Background(), trace); err != nil {
		t.Fatalf("run trace %q has no runs.db record: %v", trace, err)
	}
	// The run is an activity in the cron log, linked to the turn it drove.
	var ops []string
	es, _ := hub.Snapshot()
	for _, e := range es {
		if e.Field("event") != "cron/1001" {
			continue
		}
		ops = append(ops, e.Field("opcode"))
		if e.Source != "cron" || e.Field("trace") != trace || e.Field("activity") == "" {
			t.Errorf("job run event %+v: want the cron log, trace %s and an activity", e, trace)
		}
		if e.Field("opcode") == "start" && e.Field("related") != trace {
			t.Errorf("the run's start links to %q, want its turn %s", e.Field("related"), trace)
		}
		if e.Field("opcode") == "stop" && e.Field("duration") == "" {
			t.Error("the run's stop has no duration")
		}
	}
	if strings.Join(ops, ",") != "start,stop" {
		t.Errorf("job run opcodes = %v, want start,stop", ops)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("cron run emitted no activity events - the feed stays blind")
	}
	sawFinal := false
	for _, e := range events {
		if e.Agent != "tenant-cron" {
			t.Errorf("event %v attributed to %q, want tenant-cron", e.Kind, e.Agent)
		}
		if e.Kind == agent.EventFinal {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Error("no EventFinal - the feed never learns the run finished")
	}
}

// The old history file was one JSON array that each process rewrote whole. It
// becomes one record per line on the first load, keeping every entry — even
// one appended to it before the migration got to run (TEN-368).
func TestCronHistoryMigratesTheOldRingOnce(t *testing.T) {
	dir := t.TempDir()
	legacy := `[{"job_id":"a","started_at":"2026-09-01T09:00:00Z","duration_ms":1,"ok":true},` +
		`{"job_id":"b","started_at":"2026-09-02T09:00:00Z","duration_ms":1,"ok":false}]` +
		`{"job_id":"c","started_at":"2026-09-03T09:00:00Z","duration_ms":1,"ok":true}` + "\n"
	if err := os.WriteFile(cronHistoryPath(dir), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second load reads the migrated file: same records, no repeats
		h := loadCronHistory(dir)
		if len(h) != 3 || h[0].JobID != "a" || h[1].JobID != "b" || h[2].JobID != "c" {
			t.Fatalf("history = %+v, want a, b, c", h)
		}
	}
	raw, err := os.ReadFile(cronHistoryPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 3 || lines[0][0] != '{' {
		t.Fatalf("migrated file is not one record per line:\n%s", raw)
	}
}

// serve and the TUI append to one history while compactions rewrite it: no
// run is lost (TEN-368).
func TestCronHistoryAppendsSurviveCompaction(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan struct{})
	compacted := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				close(compacted)
				return
			default:
			}
			if err := compactCronHistory(cronHistoryPath(dir)); err != nil {
				compacted <- err
				close(compacted)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for _, who := range []string{"serve", "tui"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range cronHistoryKeep / 2 { // together, no more than a compaction keeps
				if err := appendCronHistory(dir, croneng.RunRecord{JobID: fmt.Sprintf("%s-%d", who, i)}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	if err := <-compacted; err != nil {
		t.Fatal(err)
	}
	h := loadCronHistory(dir)
	seen := map[string]bool{}
	for _, r := range h {
		seen[r.JobID] = true
	}
	if len(h) != cronHistoryKeep || len(seen) != cronHistoryKeep {
		t.Fatalf("history holds %d records (%d distinct), want %d", len(h), len(seen), cronHistoryKeep)
	}
}

// With nobody to ask, a vault secret leaves a run only into a web form or a
// command line; a sensitive read from anywhere else may be quoted where the
// run reports; a detail that names no vault secret passes.
func TestUnattendedEgress(t *testing.T) {
	detail := func(tool, sensitivity, provenance string) string {
		return tool + " arguments contain " + sensitivity + " data from " + provenance + ". This check is separate from " + tool + "'s own permission: it asks whether that data may leave, not whether the tool may run."
	}
	cases := []struct {
		tool, sensitivity, provenance string
		want                          bool
	}{
		{"web_fill", "sensitive", "vault", true}, // a login or a checkout
		{"os_exec", "sensitive", "vault", true},  // an API call with its key
		{"discord_send_message", "sensitive", "vault", false},
		{"gmail_send", "sensitive", "vault", false},
		{"x_post", "sensitive", "vault", false},
		{"web_search", "sensitive", "vault", false},
		{"web_navigate", "sensitive", "vault", false},
		{"spawn_agent", "sensitive", "vault", false},
		{"fleet_exec", "sensitive", "vault", false},
		{"twenty:create_person", "sensitive", "vault", false},
		{"discord_send_message", "sensitive", "service", true}, // a mail quoted in a report
		{"discord_send_message", "sensitive", "wiki", true},
		{"gmail_send", "untrusted", "log", true},
	}
	for _, c := range cases {
		if got := unattendedEgress(detail(c.tool, c.sensitivity, c.provenance)); got != c.want {
			t.Errorf("%s <- %s/%s: got %v, want %v", c.tool, c.sensitivity, c.provenance, got, c.want)
		}
	}
	if !unattendedEgress("discord_send_message quotes wiki") {
		t.Error("a detail that names no vault secret passes")
	}
	// The real approver refuses the same flows.
	approve := cronExecApprover(t.TempDir(), t.TempDir())
	if approve(context.Background(), "sensitive_egress", detail("discord_send_message", "sensitive", "vault")) {
		t.Error("the unattended approver let a vault secret into a Discord message")
	}
	if !approve(context.Background(), "sensitive_egress", detail("web_fill", "sensitive", "vault")) {
		t.Error("the unattended approver refused a vault secret into a web form")
	}
}

// An exec job may write its notes and its work folder although they sit under
// Tenant's install folder; config, data and the rest of the install folder
// stay protected, and a note saved through the wiki tool passes.
func TestCronExecApproverWriteRoots(t *testing.T) {
	wiki, work, cfg, data := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	ap := cronExecApproverWith(cfg, data, func() []string { return []string{wiki, work} })
	ctx := context.Background()
	for _, c := range []struct {
		action, detail string
		want           bool
	}{
		{"os_write", "write " + filepath.Join(wiki, "ops", "journal", "2026-W47.md"), true},
		{"os_edit", "edit " + filepath.Join(wiki, "ops", "journal", "2026-W47.md"), true},
		{"os_write", "OVERWRITE existing file " + filepath.Join(work, "sitefix", "audit.py"), true},
		{"wiki_save", "write sitefix/keyscan-1124.md to your local wiki", true},
		{"os_write", "write " + filepath.Join(cfg, "config.json"), false},
		{"os_write", "write " + filepath.Join(data, "cron-history.json"), false},
		{"os_write", "write " + filepath.Join(work, "..", filepath.Base(cfg), "config.json"), false},
		{"os_write", "write " + filepath.Join(exeDir(), "serve.cmd"), false},
	} {
		if got := ap(ctx, c.action, c.detail); got != c.want {
			t.Errorf("%s %q = %v, want %v", c.action, c.detail, got, c.want)
		}
	}
	// Without roots, the install folder is protected whole, as before.
	if cronExecApprover(cfg, data)(ctx, "os_write", "write "+filepath.Join(exeDir(), "work", "x.py")) {
		t.Error("no roots: a write under the install folder must still be refused")
	}
}

// An exec job's sensitive-egress gate reaches the cron approver through the
// run context (originConfirm(nil) on the exec agent), so a report built from a
// wiki read can be posted; with no approver on the context it fails closed. A
// vault secret still may not go into a message, and the cron_* tools a helper
// would see on the full plugin set stay refused.
func TestCronExecEgressAndHelpers(t *testing.T) {
	ap := cronExecApprover("/cfgdir", "/datadir")
	ctx := context.Background()
	confirm := originConfirm(nil)
	wiki := "discord_send_message arguments contain sensitive data from wiki. This check is separate"
	if !confirm(withOffsiteConfirm(ctx, ap), "sensitive_egress", wiki) {
		t.Error("the exec agent's egress confirm must reach the cron approver and allow a wiki report")
	}
	if confirm(ctx, "sensitive_egress", wiki) {
		t.Error("with no approver on the context the gate must fail closed")
	}
	vault := "discord_send_message arguments contain secret data from vault. This check is separate"
	if ap(ctx, "sensitive_egress", vault) {
		t.Error("a vault secret must not leave in a message")
	}
	for _, a := range []string{"cron_add", "cron_remove", "cron_enable"} {
		if ap(ctx, a, "a helper schedules a job") {
			t.Errorf("%s must stay refused on the exec surface, helpers included", a)
		}
	}
	if !execAllowed(model.ToolSpec{Name: "spawn_agent"}) || !execAllowed(model.ToolSpec{Name: "team_await"}) {
		t.Error("the exec surface keeps the helper tools")
	}
}

// The exec agent is built with the egress confirm.
func TestCronExecAgentConfirmsEgress(t *testing.T) {
	src, err := os.ReadFile("cronmgr.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, `AgentID: "tenant-cron-exec"`)
	if i < 0 {
		t.Fatal("no exec agent in cronmgr.go")
	}
	block := s[i:]
	if j := strings.Index(block, "})"); j > 0 {
		block = block[:j]
	}
	if !strings.Contains(block, "ConfirmSensitiveEgress: originConfirm(nil)") {
		t.Error("the exec agent must set ConfirmSensitiveEgress: originConfirm(nil)")
	}
}
