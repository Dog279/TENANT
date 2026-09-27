package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	croneng "tenant/internal/cron"
	"tenant/internal/dashboard"
	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
	"tenant/internal/model"
	cronp "tenant/internal/plugins/cron"
	"tenant/internal/runtrace"
	"tenant/internal/tui"
)

// fields renders an event's record as the tests read it: its ID, then its
// data as key=value, then its actor and surface (attributes, not data).
func fields(e *event.Event) string {
	var b strings.Builder
	b.WriteString(e.Provider + "/" + strconv.FormatUint(uint64(e.ID), 10))
	for _, f := range e.Data {
		b.WriteString(" " + f.Key + "=" + f.Text())
	}
	if e.Actor != "" || e.Surface != "" {
		b.WriteString(" actor=" + e.Actor + " surface=" + e.Surface)
	}
	return b.String()
}

// audited lists the security records captured so far.
func (c *channelEvents) audited() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Log == "security" {
			out = append(out, fields(e))
		}
	}
	return out
}

// A secret's store, change and delete are each recorded where the operator
// did it, by ID only: the value is in no record, and neither is config.json's
// diff given one.
func TestSecretChangesAreAuditedByIDOnly(t *testing.T) {
	cfg := t.TempDir()
	ev := captureChannelEvents(t)
	dash := dashKeys{cfgDir: cfg, mc: &modelControl{cfgDir: cfg}, surface: surfaceDashboard}
	tui := tuiKeys{dk: dashKeys{cfgDir: cfg, mc: dash.mc, surface: surfaceTUI}}
	for _, step := range []func() error{
		func() error { return dash.SetSecret("tavily", "tvly-first-value-123") },
		func() error { return tui.Set("tavily", "tvly-second-value-456") },
		func() error { return dash.RemoveSecret("tavily") },
		func() error { return dash.SetSecret("zai", "zai-provider-key-789") }, // a provider key: config.json changes too
		func() error { return forgetProviderSecret(cfg, "zai", surfaceCLI) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	var secrets []string
	for _, r := range ev.audited() {
		for _, v := range []string{"first-value", "second-value", "provider-key"} {
			if strings.Contains(r, v) {
				t.Errorf("a record holds a secret's value: %s", r)
			}
		}
		if !strings.HasPrefix(r, "security/4501") {
			secrets = append(secrets, r)
		}
	}
	wantLines(t, secrets,
		"security/4601 secret=tavily actor=operator surface=dashboard",
		"security/4602 secret=tavily actor=operator surface=tui",
		"security/4603 secret=tavily actor=operator surface=dashboard",
		"security/4601 secret=zai actor=operator surface=dashboard",
		"security/4603 secret=zai actor=operator surface=cli",
	)
	if len(ev.matching(catalog.ConfigChanged, "")) == 0 {
		t.Error("registering the zai provider changed config.json without a record")
	}
}

// A job added, switched and removed from the TUI, the dashboard or the
// agent's tools is recorded with who did it and from where; so is the
// cron-exec switch.
func TestScheduledJobChangesAreAuditedFromEverySurface(t *testing.T) {
	ev := captureChannelEvents(t)
	m := newCronManager(context.Background(), croneng.NewEngine(nil, nil, func([]croneng.JobDef) error { return nil }), &execGate{}, nil)
	tuiC, dashC := tuiCron{m}, dashCron{m}

	j, err := tuiC.Add(tui.CronAddSpec{Name: "brief", Spec: "0 9 * * *", Prompt: "write the brief"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tuiC.SetEnabled(j.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := tuiC.Remove(j.ID); err != nil {
		t.Fatal(err)
	}
	if err := dashC.Add(dashboard.CronAddSpec{Name: "sweep", Spec: "@daily", Prompt: "tidy up"}); err != nil {
		t.Fatal(err)
	}
	id := m.engine.List()[0].ID
	if err := dashC.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	if err := dashC.Remove(id); err != nil {
		t.Fatal(err)
	}
	if err := dashC.SetExec(true); err != nil {
		t.Fatal(err)
	}
	if err := tuiC.SetExec(false); err != nil {
		t.Fatal(err)
	}
	// The agent's tools, in a TUI turn (no surface stamped: local).
	disp := cronp.NewDispatcher(m)
	out, _, _ := disp.Dispatch(context.Background(), model.ToolCall{Name: "cron_add", Arguments: json.RawMessage(`{"spec":"@hourly","prompt":"check mail","name":"mail"}`)})
	agentID := m.engine.List()[0].ID
	for _, c := range []model.ToolCall{
		{Name: "cron_set_enabled", Arguments: json.RawMessage(`{"id":"` + agentID + `","enabled":false}`)},
		{Name: "cron_remove", Arguments: json.RawMessage(`{"id":"` + agentID + `"}`)},
	} {
		if _, isErr, err := disp.Dispatch(context.Background(), c); isErr || err != nil {
			t.Fatalf("%s: %v (after %q)", c.Name, err, out)
		}
	}
	wantLines(t, ev.audited(),
		"security/4701 job=brief id="+j.ID+" spec=0 9 * * * kind=prompt exec=false actor=operator surface=tui",
		"security/4702 job=brief id="+j.ID+" state=disabled actor=operator surface=tui",
		"security/4703 id="+j.ID+" actor=operator surface=tui",
		"security/4701 job=sweep id="+id+" spec=@daily kind=prompt exec=false actor=operator surface=dashboard",
		"security/4702 job=sweep id="+id+" state=disabled actor=operator surface=dashboard",
		"security/4703 id="+id+" actor=operator surface=dashboard",
		"security/4704 state=on actor=operator surface=dashboard",
		"security/4704 state=off actor=operator surface=tui",
		"security/4701 job=mail id="+agentID+" spec=@hourly kind=prompt exec=false actor=agent surface=local",
		"security/4702 job=mail id="+agentID+" state=disabled actor=agent surface=local",
		"security/4703 id="+agentID+" actor=agent surface=local",
	)
}

// Discovery question 6 (docs/design/logging-v2.md §9.1): "Who did this, from
// where, and with whose approval?" The agent schedules a job from a Discord
// turn; the Security log, read back from logbook.db by that turn's trace,
// answers it: the agent did it, from Discord, in that turn, and nobody
// approved it, because cron_add asks nobody (audit item D4).
func TestDiscoveryQuestion6WhoWhereAndWithWhoseApproval(t *testing.T) {
	ctx := context.Background()
	h, err := logs.Open(t.TempDir(), logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	key, err := chainKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(h.Root(), "logbook.db"), store.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	prev := logs.Default()
	logs.Install(h)
	defer func() { logs.Install(prev); _ = h.Close() }()

	m := newCronManager(ctx, croneng.NewEngine(nil, nil, func([]croneng.JobDef) error { return nil }), &execGate{}, nil)
	trace := runtrace.NewID()
	turn := withExecSurface(runtrace.WithSpan(ctx, runtrace.NewSpan(trace)), surfaceDiscord)
	if _, isErr, err := cronp.NewDispatcher(m).Dispatch(turn, model.ToolCall{Name: "cron_add",
		Arguments: json.RawMessage(`{"spec":"*/5 * * * *","prompt":"post the price","name":"ticker"}`)}); isErr || err != nil {
		t.Fatalf("cron_add: %v", err)
	}

	f, err := query.Parse("log:security trace:" + trace)
	if err != nil {
		t.Fatal(err)
	}
	evs, _, _, err := st.Query(ctx, f, store.Cursor{}, 10, false)
	if err != nil || len(evs) != 1 {
		t.Fatalf("the turn's security records: %v, %v", evs, err)
	}
	r := &evs[0]
	who, where := r.Actor, r.Surface
	if r.ID != 4701 || who != actorAgent || where != surfaceDiscord || r.Corr.TraceID != trace || r.Corr.ApprovalID != "" {
		t.Errorf("who %q, from %q, turn %q, approval %q: %s", who, where, r.Corr.TraceID, r.Corr.ApprovalID, fields(r))
	}
	if r.Msg != "Scheduled job ticker added by the agent from discord" {
		t.Errorf("message %q", r.Msg)
	}
	if v, err := st.Verify(ctx, store.VerifyOptions{}); err != nil || v.Records != 1 || v.Break != nil {
		t.Errorf("the record is not on the chain: %+v, %v", v, err)
	}
}

// A privacy export, a backup and a verify or export of the Security log each
// leave their record, naming the operator and the command.
func TestDataAndLogBookActionsAreAudited(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	c := &commonFlags{cfgDir: cfg, dataDir: data}
	var out bytes.Buffer
	if err := cmdPrivacyExport(ctx, privacyArgs(cfg, data, "--out", filepath.Join(t.TempDir(), "export")), &out); err != nil {
		t.Fatal(err)
	}
	backups := t.TempDir()
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", backups}, &out)
	})
	// The one-shot commands file their records into logbook.db, where the
	// actor and the surface are the record's own (the text line carried them).
	st, err := store.Open(filepath.Join(data, "logs", "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f, _ := query.Parse("log:security")
	evs, _, _, err := st.Query(ctx, f, store.Cursor{}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	var records []string
	for _, e := range evs {
		records = append(records, strconv.Itoa(int(e.ID))+" "+e.Actor+" "+e.Surface+" "+e.Msg[:strings.Index(e.Msg, " to ")])
	}
	wantLines(t, records, "4802 operator cli Personal data exported", "4803 operator cli Backup written")

	ev := captureChannelEvents(t) // in process, as tenant logs runs them
	if _, err := verifySecurityLog(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := exportSecurityLog(ctx, c, &bytes.Buffer{}, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	wantLines(t, ev.audited(),
		"security/4905 log=security details=2 records, 0 archives, 0 restores actor=operator surface=cli",
		"security/4903 log=security details=audit bundle actor=operator surface=cli",
	)
}
