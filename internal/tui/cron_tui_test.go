package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeCron is a stub CronControl that records calls and lets a test seed jobs
// and force errors.
type fakeCron struct {
	jobs        []CronJobView
	addCalls    int
	addSpec     string
	addPrompt   string
	addKind     string
	addExec     bool
	addTZ       string
	addErr      error
	removeCalls []string
	enableCalls []string // "id:on"
	runCalls    []string
	execOn      bool
	execCalls   []bool
}

func (f *fakeCron) Jobs() []CronJobView { return f.jobs }
func (f *fakeCron) Add(s CronAddSpec) (CronJobView, error) {
	f.addCalls++
	f.addSpec, f.addPrompt, f.addKind, f.addExec, f.addTZ = s.Spec, s.Prompt, s.Kind, s.Exec, s.TZ
	if f.addErr != nil {
		return CronJobView{}, f.addErr
	}
	v := CronJobView{ID: "job1", Name: s.Name, Spec: s.Spec, Prompt: s.Prompt, Enabled: true, Kind: s.Kind, Exec: s.Exec, TZ: s.TZ, NextRun: "2026-06-08 09:00"}
	f.jobs = append(f.jobs, v)
	return v, nil
}
func (f *fakeCron) Remove(id string) (bool, error) {
	f.removeCalls = append(f.removeCalls, id)
	return id == "job1", nil
}
func (f *fakeCron) SetEnabled(id string, on bool) (CronJobView, bool, error) {
	state := "off"
	if on {
		state = "on"
	}
	f.enableCalls = append(f.enableCalls, id+":"+state)
	return CronJobView{ID: id, Enabled: on, NextRun: "2026-06-08 09:00"}, true, nil
}
func (f *fakeCron) RunNow(id string) error {
	f.runCalls = append(f.runCalls, id)
	return nil
}
func (f *fakeCron) ExecEnabled() bool { return f.execOn }
func (f *fakeCron) SetExec(on bool) error {
	f.execCalls = append(f.execCalls, on)
	f.execOn = on
	return nil
}

func newCronModel(c CronControl) *model {
	m := newModel(context.Background(), Config{Cron: c})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func lastMsg(m *model) string { return m.msgs[len(m.msgs)-1].content }

func TestSlash_CronListEmpty(t *testing.T) {
	m := newCronModel(&fakeCron{})
	m.handleSlash("/cron")
	if got := strings.ToLower(lastMsg(m)); !strings.Contains(got, "no jobs") {
		t.Errorf("empty /cron should show a no-jobs notice; got:\n%s", lastMsg(m))
	}
}

func TestSlash_CronListShowsJobs(t *testing.T) {
	fc := &fakeCron{jobs: []CronJobView{
		{ID: "abc", Name: "nightly", Spec: "0 9 * * *", Prompt: "run the tests", Enabled: true, NextRun: "2026-06-08 09:00", LastStatus: "ok", LastRun: "2026-06-07 09:00"},
	}}
	m := newCronModel(fc)
	m.handleSlash("/cron list")
	out := lastMsg(m)
	for _, want := range []string{"nightly", "0 9 * * *", "run the tests", "abc"} {
		if !strings.Contains(out, want) {
			t.Errorf("/cron list missing %q in:\n%s", want, out)
		}
	}
}

func TestSlash_CronAddParsesPipe(t *testing.T) {
	fc := &fakeCron{}
	m := newCronModel(fc)
	m.handleSlash("/cron add 0 9 * * 1-5 | run the test suite and summarize")
	if fc.addCalls != 1 {
		t.Fatalf("addCalls=%d, want 1", fc.addCalls)
	}
	if fc.addSpec != "0 9 * * 1-5" {
		t.Errorf("spec=%q, want %q", fc.addSpec, "0 9 * * 1-5")
	}
	if fc.addPrompt != "run the test suite and summarize" {
		t.Errorf("prompt=%q", fc.addPrompt)
	}
}

func TestSlash_CronAddParsesLeadingFlags(t *testing.T) {
	fc := &fakeCron{}
	m := newCronModel(fc)
	m.handleSlash("/cron add shell exec tz=America/New_York 0 9 * * * | go test ./...")
	if fc.addCalls != 1 {
		t.Fatalf("addCalls=%d, want 1", fc.addCalls)
	}
	if fc.addKind != "shell" || !fc.addExec || fc.addTZ != "America/New_York" {
		t.Errorf("flags not parsed: kind=%q exec=%v tz=%q", fc.addKind, fc.addExec, fc.addTZ)
	}
	if fc.addSpec != "0 9 * * *" || fc.addPrompt != "go test ./..." {
		t.Errorf("spec/prompt wrong: spec=%q prompt=%q", fc.addSpec, fc.addPrompt)
	}
}

func TestParseCronAdd(t *testing.T) {
	got, _ := parseCronAdd(" exec tz=UTC 0 9 * * 1-5 ")
	if !got.Exec || got.TZ != "UTC" || got.Spec != "0 9 * * 1-5" {
		t.Errorf("parseCronAdd = %+v", got)
	}
	plain, _ := parseCronAdd("@every 30m")
	if plain.Spec != "@every 30m" || plain.Exec || plain.Kind != "" {
		t.Errorf("plain parse = %+v", plain)
	}
}

func TestSlash_CronAddRequiresPipe(t *testing.T) {
	fc := &fakeCron{}
	m := newCronModel(fc)
	m.handleSlash("/cron add 0 9 * * *") // no "|" separator
	if fc.addCalls != 0 {
		t.Errorf("add without a pipe must not reach the control (addCalls=%d)", fc.addCalls)
	}
	if !strings.Contains(strings.ToLower(lastMsg(m)), "usage") {
		t.Errorf("expected usage hint; got:\n%s", lastMsg(m))
	}
}

func TestSlash_CronAddError(t *testing.T) {
	fc := &fakeCron{addErr: errors.New("bad schedule")}
	m := newCronModel(fc)
	m.handleSlash("/cron add @every 10s | x")
	if !strings.Contains(lastMsg(m), "bad schedule") {
		t.Errorf("add error should surface; got:\n%s", lastMsg(m))
	}
}

func TestSlash_CronEnableDisableRunRemove(t *testing.T) {
	fc := &fakeCron{}
	m := newCronModel(fc)

	m.handleSlash("/cron disable job1")
	m.handleSlash("/cron enable job1")
	if len(fc.enableCalls) != 2 || fc.enableCalls[0] != "job1:off" || fc.enableCalls[1] != "job1:on" {
		t.Errorf("enable/disable calls = %v", fc.enableCalls)
	}

	m.handleSlash("/cron run job1")
	if len(fc.runCalls) != 1 || fc.runCalls[0] != "job1" {
		t.Errorf("run calls = %v", fc.runCalls)
	}

	m.handleSlash("/cron rm job1")
	if len(fc.removeCalls) != 1 || fc.removeCalls[0] != "job1" {
		t.Errorf("remove calls = %v", fc.removeCalls)
	}
}

func TestSlash_CronExecToggle(t *testing.T) {
	fc := &fakeCron{}
	m := newCronModel(fc)

	// status when off
	m.handleSlash("/cron exec")
	if !strings.Contains(strings.ToLower(lastMsg(m)), "off") {
		t.Errorf("exec status (off) wrong: %s", lastMsg(m))
	}
	// turn on
	m.handleSlash("/cron exec on")
	if len(fc.execCalls) != 1 || fc.execCalls[0] != true || !fc.execOn {
		t.Fatalf("exec on not wired: calls=%v on=%v", fc.execCalls, fc.execOn)
	}
	// turn off
	m.handleSlash("/cron exec off")
	if len(fc.execCalls) != 2 || fc.execCalls[1] != false || fc.execOn {
		t.Fatalf("exec off not wired: calls=%v on=%v", fc.execCalls, fc.execOn)
	}
	// bad arg → usage
	m.handleSlash("/cron exec maybe")
	if !strings.Contains(strings.ToLower(lastMsg(m)), "usage") {
		t.Errorf("bad exec arg should show usage: %s", lastMsg(m))
	}
}

func TestSlash_CronListShowsExecState(t *testing.T) {
	fc := &fakeCron{execOn: true}
	m := newCronModel(fc)
	m.handleSlash("/cron")
	if !strings.Contains(strings.ToLower(lastMsg(m)), "exec: on") {
		t.Errorf("list header should show exec state; got:\n%s", lastMsg(m))
	}
}

func TestSlash_CronUnavailable(t *testing.T) {
	m := newModel(context.Background(), Config{}) // no Cron control
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/cron")
	if !strings.Contains(strings.ToLower(lastMsg(m)), "not available") {
		t.Errorf("nil Cron control should say unavailable; got:\n%s", lastMsg(m))
	}
}
