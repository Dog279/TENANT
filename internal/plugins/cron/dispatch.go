// Package cron is the LLM-facing tool surface for managing recurring jobs
// (cron_list / cron_add / cron_remove / cron_set_enabled / cron_run_now). It is
// deliberately decoupled from the scheduler engine (internal/cron) and the CLI:
// it talks to a small Manager interface that cmd/tenant satisfies, so there is
// no import cycle and the plugin can be unit-tested with a fake.
//
// The mutating tools are Gated (write/blast-radius: scheduling autonomous future
// work). The cron runner itself never sees these tools — they are cut from its
// surface — so a scheduled job cannot schedule more jobs.
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// JobView is the plugin's flat, render-ready view of one job.
type JobView struct {
	ID         string
	Name       string
	Spec       string
	Prompt     string
	Enabled    bool
	Kind       string // "prompt" | "shell"
	Exec       bool   // prompt jobs: runs with the exec (dangerous) tool surface
	TZ         string // "" = engine default
	NextRun    string // human-readable, "" when not scheduled
	LastRun    string
	LastStatus string
}

// AddSpec is the input to AddJob (a struct so new fields don't churn callers).
type AddSpec struct {
	Name    string
	Spec    string
	Prompt  string
	Enabled bool
	Kind    string // "" / "prompt" | "shell"
	Exec    bool
	TZ      string
}

// Manager is the slice of the cron engine the tools drive. cmd/tenant's
// cronManager implements it over *cron.Engine.
type Manager interface {
	ListJobs() []JobView
	AddJob(AddSpec) (JobView, error)
	RemoveJob(id string) (removed bool, err error)
	SetJobEnabled(id string, on bool) (view JobView, changed bool, err error)
	RunJobNow(ctx context.Context, id string) (summary string, err error)
}

// Dispatcher implements agent.ToolDispatcher for cron management.
type Dispatcher struct{ mgr Manager }

// NewDispatcher wires a dispatcher over a Manager.
func NewDispatcher(mgr Manager) *Dispatcher { return &Dispatcher{mgr: mgr} }

const specHelp = "Schedule: standard 5-field crontab (\"m h dom mon dow\", e.g. \"0 9 * * 1-5\" = 9am Mon-Fri), " +
	"or @every <dur> (e.g. \"@every 30m\", minimum 1m), or a macro (@hourly/@daily/@weekly/@monthly)."

// Tools returns the cron management tool specs. cron_list is read-only; the
// mutating tools are Gated (each requires operator approval in an interactive
// session, and is cut entirely from an unattended cron run).
func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		{Name: "cron_list",
			Description: "List the scheduled recurring jobs (id, name, schedule, next run, last status). Read-only.",
			Parameters:  obj(``)},
		{Name: "cron_add",
			Description: "Schedule a new recurring job. By default the job runs an agent prompt UNATTENDED and read/comms-safe (cannot exec, write, send, or schedule more jobs). " +
				"DANGEROUS opt-ins (require the operator to have enabled cron-exec globally): kind=\"shell\" runs a shell command; exec=true gives a prompt job the dangerous tool surface. Prefer the safe default. " + specHelp,
			Parameters: obj(`"spec":{"type":"string","description":"the schedule, e.g. \"0 9 * * 1-5\" or \"@every 30m\""},`+
				`"prompt":{"type":"string","description":"the instruction (kind=prompt) or the shell command (kind=shell) to run each time"},`+
				`"name":{"type":"string","description":"optional short label"},`+
				`"enabled":{"type":"boolean","description":"start enabled (default true)"},`+
				`"kind":{"type":"string","enum":["prompt","shell"],"description":"prompt (default, safe agent turn) or shell (runs a command, DANGEROUS)"},`+
				`"exec":{"type":"boolean","description":"prompt jobs only: run with the exec/dangerous tool surface, auto-approved unattended (DANGEROUS; default false)"},`+
				`"tz":{"type":"string","description":"optional IANA timezone, e.g. America/New_York (default: server timezone)"}`, "spec", "prompt"),
			Gated: true},
		{Name: "cron_remove",
			Description: "Delete a scheduled job by id.",
			Parameters:  obj(`"id":{"type":"string","description":"the job id from cron_list"}`, "id"), Gated: true, Risk: model.RiskDestructive},
		{Name: "cron_set_enabled",
			Description: "Enable or disable a scheduled job by id.",
			Parameters: obj(`"id":{"type":"string","description":"the job id"},`+
				`"enabled":{"type":"boolean","description":"true to enable, false to disable"}`, "id", "enabled"),
			Gated: true},
		{Name: "cron_run_now",
			Description: "Run a scheduled job once right now (does not change its schedule). Returns the run summary.",
			Parameters:  obj(`"id":{"type":"string","description":"the job id"}`, "id"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if d.mgr == nil {
		return "cron management is not available in this session", true, nil
	}
	switch call.Name {
	case "cron_list":
		return d.list()
	case "cron_add":
		return d.add(call.Arguments)
	case "cron_remove":
		return d.remove(call.Arguments)
	case "cron_set_enabled":
		return d.setEnabled(call.Arguments)
	case "cron_run_now":
		return d.runNow(ctx, call.Arguments)
	default:
		return "unknown cron tool: " + call.Name, true, nil
	}
}

func (d *Dispatcher) list() (string, bool, error) {
	jobs := d.mgr.ListJobs()
	if len(jobs) == 0 {
		return "(no scheduled jobs)", false, nil
	}
	var b strings.Builder
	for _, j := range jobs {
		state := "enabled"
		if !j.Enabled {
			state = "disabled"
		}
		name := j.Name
		if name == "" {
			name = "(unnamed)"
		}
		mode := j.Kind
		if mode == "" {
			mode = "prompt"
		}
		if j.Exec {
			mode += "+exec"
		}
		if j.TZ != "" {
			mode += " " + j.TZ
		}
		fmt.Fprintf(&b, "%s  %s  [%s %s]  spec=%q  next=%s", j.ID, name, state, mode, j.Spec, orDash(j.NextRun))
		if j.LastStatus != "" {
			fmt.Fprintf(&b, "  last=%s@%s", j.LastStatus, orDash(j.LastRun))
		}
		fmt.Fprintf(&b, "\n  prompt: %s\n", j.Prompt)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) add(args json.RawMessage) (string, bool, error) {
	var a struct {
		Spec    string `json:"spec"`
		Prompt  string `json:"prompt"`
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		TZ      string `json:"tz"`
		Enabled *bool  `json:"enabled"`
		Exec    *bool  `json:"exec"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Spec) == "" {
		return "spec is required (" + specHelp + ")", true, nil
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "prompt (or command, for kind=shell) is required", true, nil
	}
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	exec := false
	if a.Exec != nil {
		exec = *a.Exec
	}
	j, err := d.mgr.AddJob(AddSpec{
		Name: a.Name, Spec: a.Spec, Prompt: a.Prompt, Kind: a.Kind, TZ: a.TZ, Enabled: enabled, Exec: exec,
	})
	if err != nil {
		return "could not add job: " + err.Error(), true, nil
	}
	mode := j.Kind
	if j.Exec {
		mode += " +exec"
	}
	return fmt.Sprintf("scheduled %s job %s (next run: %s)", mode, j.ID, orDash(j.NextRun)), false, nil
}

func (d *Dispatcher) remove(args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.ID) == "" {
		return "id is required", true, nil
	}
	removed, err := d.mgr.RemoveJob(a.ID)
	if err != nil {
		return "could not remove job: " + err.Error(), true, nil
	}
	if !removed {
		return "no job with id " + a.ID, false, nil
	}
	return "removed job " + a.ID, false, nil
}

func (d *Dispatcher) setEnabled(args json.RawMessage) (string, bool, error) {
	var a struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.ID) == "" {
		return "id is required", true, nil
	}
	if a.Enabled == nil {
		return "enabled is required (true or false)", true, nil
	}
	j, changed, err := d.mgr.SetJobEnabled(a.ID, *a.Enabled)
	if err != nil {
		return "could not update job: " + err.Error(), true, nil
	}
	if !changed {
		return "no change (job not found or already in that state)", false, nil
	}
	state := "enabled"
	if !j.Enabled {
		state = "disabled"
	}
	return fmt.Sprintf("job %s %s (next run: %s)", j.ID, state, orDash(j.NextRun)), false, nil
}

func (d *Dispatcher) runNow(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.ID) == "" {
		return "id is required", true, nil
	}
	summary, err := d.mgr.RunJobNow(ctx, a.ID)
	if err != nil {
		return "run failed: " + err.Error(), true, nil
	}
	if summary == "" {
		summary = "(no output)"
	}
	return "ran job " + a.ID + ": " + summary, false, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
