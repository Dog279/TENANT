package cron

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
)

// fakeManager records calls and returns canned results.
type fakeManager struct {
	jobs      []JobView
	addErr    error
	addCalls  int
	lastAdd   AddSpec
	rmCalls   int
	enCalls   int
	runCalls  int
	runResult string
}

func (f *fakeManager) ListJobs() []JobView { return f.jobs }
func (f *fakeManager) AddJob(s AddSpec) (JobView, error) {
	f.addCalls++
	f.lastAdd = s
	if f.addErr != nil {
		return JobView{}, f.addErr
	}
	return JobView{ID: "abc123", Name: s.Name, Spec: s.Spec, Prompt: s.Prompt, Enabled: s.Enabled, Kind: s.Kind, Exec: s.Exec, TZ: s.TZ, NextRun: "soon"}, nil
}
func (f *fakeManager) RemoveJob(id string) (bool, error) { f.rmCalls++; return id == "abc123", nil }
func (f *fakeManager) SetJobEnabled(id string, on bool) (JobView, bool, error) {
	f.enCalls++
	return JobView{ID: id, Enabled: on, NextRun: "soon"}, true, nil
}
func (f *fakeManager) RunJobNow(ctx context.Context, id string) (string, error) {
	f.runCalls++
	return f.runResult, nil
}

func call(name, args string) model.ToolCall {
	return model.ToolCall{Name: name, Arguments: json.RawMessage(args)}
}

func TestToolsGating(t *testing.T) {
	d := NewDispatcher(&fakeManager{})
	gated := map[string]bool{}
	for _, sp := range d.Tools() {
		gated[sp.Name] = sp.Gated
	}
	if gated["cron_list"] {
		t.Error("cron_list must be ungated (read-only)")
	}
	for _, n := range []string{"cron_add", "cron_remove", "cron_set_enabled", "cron_run_now"} {
		if !gated[n] {
			t.Errorf("%s must be Gated (blast-radius write)", n)
		}
	}
}

func TestDispatchAddValidation(t *testing.T) {
	f := &fakeManager{}
	d := NewDispatcher(f)
	// Missing spec.
	if out, isErr, _ := d.Dispatch(context.Background(), call("cron_add", `{"prompt":"x"}`)); !isErr || !strings.Contains(out, "spec is required") {
		t.Errorf("missing spec: out=%q isErr=%v", out, isErr)
	}
	// Missing prompt.
	if out, isErr, _ := d.Dispatch(context.Background(), call("cron_add", `{"spec":"@every 5m"}`)); !isErr || !strings.Contains(out, "is required") {
		t.Errorf("missing prompt: out=%q isErr=%v", out, isErr)
	}
	if f.addCalls != 0 {
		t.Error("validation failures must not reach the manager")
	}
	// Valid.
	out, isErr, _ := d.Dispatch(context.Background(), call("cron_add", `{"spec":"@every 5m","prompt":"do x","name":"n"}`))
	if isErr || !strings.Contains(out, "abc123") {
		t.Errorf("valid add: out=%q isErr=%v", out, isErr)
	}
	if f.addCalls != 1 {
		t.Errorf("addCalls=%d, want 1", f.addCalls)
	}
}

func TestDispatchAddParsesKindExecTZ(t *testing.T) {
	f := &fakeManager{}
	d := NewDispatcher(f)
	out, isErr, _ := d.Dispatch(context.Background(),
		call("cron_add", `{"spec":"0 9 * * *","prompt":"go test ./...","kind":"shell","exec":true,"tz":"America/New_York"}`))
	if isErr {
		t.Fatalf("valid shell add errored: %q", out)
	}
	if f.lastAdd.Kind != "shell" || !f.lastAdd.Exec || f.lastAdd.TZ != "America/New_York" {
		t.Errorf("kind/exec/tz not threaded: %+v", f.lastAdd)
	}
}

func TestDispatchListEmptyAndPopulated(t *testing.T) {
	d := NewDispatcher(&fakeManager{})
	if out, _, _ := d.Dispatch(context.Background(), call("cron_list", `{}`)); !strings.Contains(out, "no scheduled jobs") {
		t.Errorf("empty list: %q", out)
	}
	d2 := NewDispatcher(&fakeManager{jobs: []JobView{
		{ID: "x1", Name: "nightly", Spec: "0 9 * * *", Prompt: "run tests", Enabled: true, NextRun: "tomorrow 9am", LastStatus: "ok", LastRun: "today"},
	}})
	out, _, _ := d2.Dispatch(context.Background(), call("cron_list", `{}`))
	for _, want := range []string{"x1", "nightly", "0 9 * * *", "run tests", "ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q in: %s", want, out)
		}
	}
}

func TestDispatchRemoveAndEnableAndRun(t *testing.T) {
	f := &fakeManager{runResult: "all green"}
	d := NewDispatcher(f)

	if out, _, _ := d.Dispatch(context.Background(), call("cron_remove", `{"id":"abc123"}`)); !strings.Contains(out, "removed") {
		t.Errorf("remove: %q", out)
	}
	if out, _, _ := d.Dispatch(context.Background(), call("cron_remove", `{"id":"nope"}`)); !strings.Contains(out, "no job") {
		t.Errorf("remove missing: %q", out)
	}
	if out, _, _ := d.Dispatch(context.Background(), call("cron_set_enabled", `{"id":"x","enabled":false}`)); !strings.Contains(out, "disabled") {
		t.Errorf("set_enabled: %q", out)
	}
	// enabled missing -> error, no manager call counted beyond the prior one
	if out, isErr, _ := d.Dispatch(context.Background(), call("cron_set_enabled", `{"id":"x"}`)); !isErr || !strings.Contains(out, "enabled is required") {
		t.Errorf("set_enabled missing flag: %q isErr=%v", out, isErr)
	}
	if out, _, _ := d.Dispatch(context.Background(), call("cron_run_now", `{"id":"x"}`)); !strings.Contains(out, "all green") {
		t.Errorf("run_now: %q", out)
	}
	if f.runCalls != 1 {
		t.Errorf("runCalls=%d, want 1", f.runCalls)
	}
}

func TestDispatchNilManager(t *testing.T) {
	d := NewDispatcher(nil)
	if out, isErr, _ := d.Dispatch(context.Background(), call("cron_list", `{}`)); !isErr || !strings.Contains(out, "not available") {
		t.Errorf("nil manager: %q isErr=%v", out, isErr)
	}
}

func TestDispatchUnknownTool(t *testing.T) {
	d := NewDispatcher(&fakeManager{})
	if out, isErr, _ := d.Dispatch(context.Background(), call("cron_bogus", `{}`)); !isErr || !strings.Contains(out, "unknown cron tool") {
		t.Errorf("unknown: %q isErr=%v", out, isErr)
	}
}
