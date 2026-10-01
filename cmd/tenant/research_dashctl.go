package main

// research_dashctl.go: the dashboard's Research page (TEN-210). Serve and
// the terminal app each give it a research-only runtime copied from their
// team config; the terminal app's copy shares its researchControl's store and
// one-run flag. Either way a run goes through researchControl.runOne, the
// same path /research takes, under the process lifetime (not a request), so
// closing the tab never stops it.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/lifecycle"
	"tenant/internal/orchestra"
	"tenant/internal/research"
	"tenant/internal/tui"
)

// newDashResearch adapts the terminal app's rc; next streams progress lines
// to its feed. It gives rc the one-run flag the two sides share, and the page
// its own runtime from cfg, as serve's and the terminal's: a research
// window on the orchestrator's runtime would cancel a chat's or a relay's
// helpers (C-1). nil (an untyped nil interface) leaves the page showing
// "isn't available".
func newDashResearch(base context.Context, life *lifecycle.Group, cfg TeamConfig, shared *TeamRuntime,
	rc *researchControl, web bool, next func(string, ...any)) dashboard.ResearchControl {
	if rc == nil {
		return nil
	}
	if rc.busy == nil {
		rc.busy = new(atomic.Bool) // rc is the terminal app's own: its runs share the flag
	}
	own := *rc
	cfg.Web = web
	own.rt = ownResearchRuntime(life, cfg)
	d := dashResearchOf(base, &own, web, next)
	d.before = func() { own.rt.SetAgentProfiles(shared.AgentProfiles()) }
	return d
}

// ownResearchRuntime is a runtime for research alone: a research Begin or
// EndRun on the shared runtime would cancel the helpers of chat, the Discord
// relay and scheduled jobs, so research gets its own bus and runtime. The
// tools, the approval gate and the web policy are the shared ones.
func ownResearchRuntime(life *lifecycle.Group, cfg TeamConfig) *TeamRuntime {
	cfg.Bus = orchestra.NewBusWithHistoryCap(cfg.Budgets.BusHistoryCap)
	cfg.Bus.Register(cfg.OrchID)
	life.AddFunc(lifecycle.Release, "research bus", cfg.Bus.Close)
	cfg.LocalRestore, cfg.LocalOnChange = nil, nil // the orchestrator's tool switches are its own to save
	rt := newTeamRuntime(cfg)
	life.AddFunc(lifecycle.Release, "research team", rt.Close)
	return rt
}

// runsAsService is runAsService; a test swaps it to try both accounts.
var runsAsService = runAsService

// newServeDashResearch builds serve's research: its own runtime from cfg,
// sharing shared's tools. Web browsing goes off when cfg.WebConfig.NoSandbox
// && runAsService(): a Windows service account, or one the OS won't name (D19).
func newServeDashResearch(base context.Context, life *lifecycle.Group, cfg TeamConfig, shared *TeamRuntime,
	rc researchControl, dataDir string, web bool) dashboard.ResearchControl {
	why := ""
	if web && cfg.WebConfig.NoSandbox && runsAsService() {
		web, why = false, "Web browsing is off for research on this server because the browser would have full control of the computer."
	}
	cfg.Web = web
	rc.rt = ownResearchRuntime(life, cfg)
	store, err := research.New(dataDir)
	if err != nil {
		sysUnavailable.Log(base, "component", "research store", "effect", "research history is off", "err", err)
		store = nil
	}
	rc.store = store
	d := dashResearchOf(base, &rc, web, nil)
	d.live.WebOffWhy = why
	// Helpers edited on the Helpers page change the shared runtime's
	// profiles; the research runtime picks them up at each start.
	d.before = func() { rc.rt.SetAgentProfiles(shared.AgentProfiles()) }
	return d
}

// dashResearch is dashboard.ResearchControl over a researchControl copy
// whose progress sinks (say, emitTimeline) feed a live snapshot the page
// streams. The copy never feeds the terminal app's timeline pane, which
// would open for a run it didn't start and never close.
type dashResearch struct {
	rc     researchControl
	base   context.Context      // the process lifetime, never a request
	next   func(string, ...any) // the terminal app's feed; nil under serve
	before func()               // serve: sync helper profiles before a run
	run    func(ctx context.Context, q, replayOf string, skip bool) (id, report string, err error)

	since time.Time // when this process's runs began: an older "running" row is one a restart cut short

	mu      sync.Mutex
	live    dashboard.ResearchLiveView
	started time.Time
	cancel  context.CancelFunc
	changed chan struct{} // closed and replaced by bump, under mu
}

func dashResearchOf(base context.Context, rc *researchControl, web bool, next func(string, ...any)) *dashResearch {
	if rc.busy == nil {
		rc.busy = new(atomic.Bool)
	}
	d := &dashResearch{rc: *rc, base: base, next: next, since: time.Now(), changed: make(chan struct{})}
	d.rc.say, d.rc.emitTimeline = d.say, d.timeline
	d.run = d.rc.runOne
	d.live = dashboard.ResearchLiveView{SavesToNotes: func() bool { d, _ := rc.wiki.get(); return d != "" }(), WebOff: !web, HistoryOn: rc.store != nil}
	return d
}

// bump wakes every Watch; mu is held.
func (d *dashResearch) bump() {
	close(d.changed)
	d.changed = make(chan struct{})
}

func (d *dashResearch) Watch() (dashboard.ResearchLiveView, <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.live
	v.Lines = slices.Clone(v.Lines)
	v.Elsewhere = !v.Running && d.rc.busy.Load()
	if v.Running {
		v.Elapsed = researchDur(time.Since(d.started))
	}
	return v, d.changed
}

// say is the pipeline's progress line: kept for the page in plain words,
// and passed on to the terminal feed as it is.
func (d *dashResearch) say(format string, a ...any) {
	d.mu.Lock()
	if format == sayRunID && len(a) == 1 {
		d.live.RunID, _ = a[0].(string)
	}
	if line := plainLine(format, a); line != "" {
		d.live.Lines = append(d.live.Lines[max(0, len(d.live.Lines)-19):], line)
	}
	d.bump()
	d.mu.Unlock()
	if d.next != nil {
		d.next(format, a...)
	}
}

// plainLine is the page's wording of the few progress lines worth showing,
// or "" for the rest: the phase, round and questions already say those, and
// the terminal feed keeps them all. Error text stays off the page (D21).
func plainLine(format string, a []any) string {
	switch format {
	case "  · [%s] %s — %d chars: %s": // id, status, length, preview
		if a[1] == "done" {
			return fmt.Sprintf("A helper finished: %v", a[3])
		}
		return "A helper didn't finish."
	case "📄 saved to wiki: %s":
		return fmt.Sprintf("Saved to your agent's notes as %v.", a[0])
	case "⚠ couldn't save to wiki: %v":
		return "Couldn't save the report to your agent's notes."
	case "time cap reached — finishing with current findings":
		return "The time limit was reached, so the report uses what was found so far."
	case "⏱  wave timed out — salvaged %d stuck agent(s)":
		return fmt.Sprintf("%v helpers took too long and were asked to write up what they had.", a[0])
	case "  ! spawn failed: %v":
		return "A helper couldn't start."
	}
	return ""
}

// timeline turns the pipeline's structured updates into the page's phase.
func (d *dashResearch) timeline(u tui.ResearchTimelineUpdate) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch u.Kind {
	case "started":
		d.live.Phase = "planning"
	case "plan":
		d.live.Phase, d.live.Round, d.live.Rounds, d.live.Progress = "researching", u.Cycle, u.Total, ""
		if u.Plan != nil {
			d.live.Questions = u.Plan.SubQuestions
		}
	case "wave":
		if w := u.Wave; w != nil && w.From == w.To {
			d.live.Progress = fmt.Sprintf("Question %d is in progress.", w.From)
		} else if w != nil {
			d.live.Progress = fmt.Sprintf("Questions %d-%d are in progress.", w.From, w.To)
		}
	case "reflect_done":
		d.live.Phase = "reflecting"
	case "synth":
		d.live.Phase = "writing"
	case "done":
		if u.Done != nil && u.Done.Status == "done" {
			d.live.Phase = "saving" // the notes copy is written after this
		}
	default: // agent_status: a helper's row, which the page doesn't list
		return
	}
	d.bump()
}

func (d *dashResearch) Start(question string, skipQuestions bool) error {
	return d.launch(question, "", skipQuestions)
}

// launch starts a run in the background. One at a time: the terminal app's
// runs hold the same flag (runOne takes it; a lost race ends "failed" with
// the busy text).
func (d *dashResearch) launch(q, replayOf string, skip bool) error {
	d.mu.Lock()
	if d.live.Running {
		d.mu.Unlock()
		return dashboard.PlainError("Research is already running. Watch it below or stop it.")
	}
	if d.rc.busy.Load() {
		d.mu.Unlock()
		return dashboard.PlainError("Research is running from the terminal. Start another when it finishes.")
	}
	ctx, cancel := context.WithCancel(d.base)
	d.started, d.cancel = time.Now(), cancel
	phase := "checking" // the clarifier runs first
	if skip {
		phase = "planning" // a re-run, a clarified question, or "exactly what I typed"
	}
	d.live = dashboard.ResearchLiveView{
		SavesToNotes: d.live.SavesToNotes, WebOff: d.live.WebOff, HistoryOn: d.live.HistoryOn, WebOffWhy: d.live.WebOffWhy,
		Phase: phase, Running: true, Question: q, ReplayOf: replayOf,
	}
	d.bump()
	d.mu.Unlock()
	if d.before != nil {
		d.before()
	}
	go func() {
		defer cancel()
		id, report, err := d.run(ctx, q, replayOf, skip)
		d.finish(ctx, q, id, report, err)
	}()
	return nil
}

// finish records how a run ended.
func (d *dashResearch) finish(ctx context.Context, q, id, report string, err error) {
	var done *dashboard.ResearchReportView
	if err == nil {
		done = d.doneReport(id, q, report) // reads the store: outside mu
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var clarify *ClarifyNeededError
	switch {
	case err == nil:
		d.live.Phase, d.live.Report = "done", done
	case errors.As(err, &clarify):
		d.live.Phase, d.live.Clarify, d.live.Question = "clarify", clarify.Questions, clarify.Question
	case ctx.Err() != nil:
		d.live.Phase = "stopped"
	default:
		d.live.Phase, d.live.Err = "failed", plainResearchErr(err.Error())
	}
	if id != "" {
		d.live.RunID = id
	}
	d.live.Running, d.live.Elapsed, d.cancel = false, researchDur(time.Since(d.started)), nil
	d.bump()
}

// doneReport is a finished run's report: the stored one, or, without a
// store, the one in hand.
func (d *dashResearch) doneReport(id, q, report string) *dashboard.ResearchReportView {
	if d.rc.store != nil && id != "" {
		if v, err := d.Report(id); err == nil {
			return &v
		}
	}
	return &dashboard.ResearchReportView{ID: id, Question: q, StateText: "Finished",
		Body: reportBody(report), Refs: researchRefs(parseReferencesURIs(report))}
}

// plainResearchErr says why a run failed, in plain words; the raw error is
// in the log (research.RunActivity records it).
func plainResearchErr(msg string) string {
	switch {
	case msg == errResearchBusy.Error():
		return "Research was already running from the terminal. Start another when it finishes."
	case strings.HasPrefix(msg, "resolve planner"), strings.HasPrefix(msg, "research: plan"):
		return "Your agent's model didn't answer. Check the Model page, then try again."
	case strings.Contains(msg, "no usable findings"):
		return "The helpers didn't find enough to write a report. What they found is listed with the run."
	}
	return "Research stopped because of an error. The full log has the details."
}

func (d *dashResearch) Clarify(answer string) error {
	d.mu.Lock()
	phase, q := d.live.Phase, d.live.Question
	d.mu.Unlock()
	if phase != "clarify" {
		return dashboard.PlainError("There's no question waiting for your answer.")
	}
	return d.launch(EnrichClarified(q, answer), "", true)
}

// Replay is ResearchReplay run in the background: the same question on the
// current model, marked as a re-run of id.
func (d *dashResearch) Replay(id string) error {
	if d.rc.store == nil {
		return errNoResearchHistory
	}
	m, _, err := d.rc.store.Get(id)
	if err != nil {
		return notInList(err)
	}
	return d.launch(m.Question, id, true)
}

func (d *dashResearch) Stop() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.live.Running || d.cancel == nil {
		return false
	}
	d.cancel() // the helpers stop with it: the run context drives Await and the team's budget
	return true
}

var errNoResearchHistory = dashboard.PlainError("Research history isn't available on this Tenant.")

// notInList phrases a store read that failed.
func notInList(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return dashboard.PlainError("That research isn't in the list any more.")
	}
	return err
}

func (d *dashResearch) History(limit int) ([]dashboard.ResearchRunRow, error) {
	if d.rc.store == nil {
		return nil, errNoResearchHistory
	}
	ms, err := d.rc.store.List(limit)
	if err != nil {
		return nil, err
	}
	rows := make([]dashboard.ResearchRunRow, 0, len(ms))
	for _, m := range ms {
		text, live := d.stateOf(m)
		rows = append(rows, dashboard.ResearchRunRow{
			ID: m.ID, Question: m.Question, State: string(m.Status), StateText: text,
			When: m.Started.Local().Format("Jan 2, 15:04"), Took: tookOf(m), Model: m.Model, ReplayOf: m.ReplayOf,
			Sources: len(m.References), Live: live,
		})
	}
	return rows, nil
}

// stateOf is a stored run's state in plain words, and whether it is going
// now. A stored "running" is the page's live run, the terminal app's (it
// holds the shared flag, and this process made the row), or one a restart
// cut short.
func (d *dashResearch) stateOf(m research.Manifest) (text string, live bool) {
	switch {
	case m.Status == research.StatusRunning:
		d.mu.Lock()
		mine := d.live.Running && m.ID == d.live.RunID
		terminal := !d.live.Running && d.rc.busy.Load() && !m.Started.Before(d.since)
		d.mu.Unlock()
		switch {
		case mine:
			return "Running now", true
		case terminal:
			return "Running from the terminal", true
		}
		return "Interrupted (Tenant restarted)", false
	case m.Status == research.StatusDone:
		return "Finished", false
	case m.Status == research.StatusPartial:
		return "Partly finished", false
	case strings.HasPrefix(m.ErrorMessage, "research: clarification needed"):
		return "Asked for more detail", false
	case strings.Contains(m.ErrorMessage, context.Canceled.Error()):
		return "Stopped", false
	}
	return "Didn't finish", false
}

func tookOf(m research.Manifest) string {
	if m.Finished.IsZero() {
		return ""
	}
	return researchDur(m.Finished.Sub(m.Started))
}

func (d *dashResearch) Report(id string) (dashboard.ResearchReportView, error) {
	if d.rc.store == nil {
		return dashboard.ResearchReportView{}, errNoResearchHistory
	}
	m, body, err := d.rc.store.Get(id)
	if err != nil {
		return dashboard.ResearchReportView{}, notInList(err)
	}
	v := dashboard.ResearchReportView{
		ID: m.ID, Question: m.Question,
		When: m.Started.Local().Format("Jan 2, 15:04"), Took: tookOf(m), Model: m.Model, ReplayOf: m.ReplayOf,
		Body: reportBody(body), Refs: researchRefs(m.References),
	}
	v.StateText, v.Live = d.stateOf(m)
	if v.StateText == "Didn't finish" || v.StateText == "Partly finished" { // not a question back, and not a stop
		v.Err = plainResearchErr(m.ErrorMessage)
	}
	for _, f := range m.Findings {
		rf := dashboard.ResearchFinding{Question: subQuestionOf(f.Task), State: "Didn't finish"}
		if f.Status == "done" { // anything else holds an error, which stays off the page
			rf.State = "Finished"
			rf.Body, _ = d.rc.store.GetFinding(id, f.AgentID)
		}
		v.Findings = append(v.Findings, rf)
	}
	return v, nil
}

// Delete removes a run from the list. The copy in the agent's notes stays.
func (d *dashResearch) Delete(id string) error {
	if d.rc.store == nil {
		return errNoResearchHistory
	}
	if m, _, err := d.rc.store.Get(id); err == nil {
		if _, live := d.stateOf(m); live { // its folder is still being written
			return dashboard.PlainError("That research is still running. Stop it first, then delete it.")
		}
	}
	return d.rc.ResearchDelete(id)
}

// reportBody is a report without its reference block (the page lists the
// references itself).
func reportBody(report string) string {
	if i := strings.LastIndex(report, "## References"); i >= 0 {
		report = report[:i]
	}
	return strings.TrimSpace(report)
}

func researchRefs(uris []string) []dashboard.ResearchRef {
	refs := make([]dashboard.ResearchRef, len(uris))
	for i, u := range uris {
		refs[i] = dashboard.ResearchRef{N: i + 1, URL: u}
	}
	return refs
}

// subQuestionLead is the line researcherTask puts a helper's question on.
const subQuestionLead = "YOUR sub-question to investigate (focus only on this): "

// subQuestionOf is the question a helper was given, read back from its task.
func subQuestionOf(task string) string {
	if _, q, ok := strings.Cut(task, subQuestionLead); ok {
		task = q
	}
	q, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	return strings.TrimSpace(q)
}

// researchDur is a duration in words: "less than a minute", "4 min",
// "1 h 5 min".
func researchDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return fmt.Sprintf("%d h %d min", d/time.Hour, d%time.Hour/time.Minute)
}
