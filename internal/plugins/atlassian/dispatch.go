package atlassian

// dispatch.go exposes the Jira tools to the agent + the blast-radius gate
// (TEN-54), same shape as the gsuite/sql plugins. Reads (search/get/transitions)
// are always allowed; writes (create/comment/transition) MODIFY the shared board
// so they're denied unless AllowWrite is set or a per-action Confirm approves —
// deny-by-default. The model cannot change the policy.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// Policy gates Jira writes by blast radius.
type Policy struct {
	AllowWrite bool
	Confirm    func(ctx context.Context, action, detail string) bool
}

type actionClass int

const (
	classRead actionClass = iota
	classWrite
)

func (p Policy) gate(ctx context.Context, c actionClass, detail string) error {
	if c == classRead || p.AllowWrite {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, "jira_write", detail) {
		return nil
	}
	return fmt.Errorf("blocked: this would modify the Jira board (%s) and was not approved — "+
		"enable the write flag or confirm. This is a blast-radius boundary, not a bug", detail)
}

// Dispatcher implements the agent tool-dispatcher for Atlassian/Jira.
type Dispatcher struct {
	svc    *Service
	policy Policy
}

func NewDispatcher(svc *Service, policy Policy) *Dispatcher {
	return &Dispatcher{svc: svc, policy: policy}
}

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
		{Name: "jira_search", Description: "Search Jira issues with JQL (e.g. `project = TEN AND statusCategory != Done ORDER BY created DESC`). Returns key/summary/status/type/assignee.",
			Parameters: obj(`"jql":{"type":"string"},"max":{"type":"integer","description":"max results (default 25, cap 50)"}`, "jql")},
		{Name: "jira_get", Description: "Get one Jira issue by key or id (e.g. TEN-16). Returns its fields incl. description.",
			Parameters: obj(`"key":{"type":"string"}`, "key")},
		{Name: "jira_transitions", Description: "List the workflow transitions currently available on an issue (id + name + target status). Use before jira_transition.",
			Parameters: obj(`"key":{"type":"string"}`, "key")},
		{Name: "jira_create", Description: "Create a Jira issue. GATED (writes to the shared board). Defaults: project = the configured default, type = Task.",
			Parameters: obj(`"summary":{"type":"string"},"description":{"type":"string"},"project":{"type":"string","description":"project key; omit for default"},"type":{"type":"string","description":"issue type; default Task"}`, "summary"), Gated: true},
		{Name: "jira_comment", Description: "Add a plain-text comment to an issue. GATED (writes to the shared board).",
			Parameters: obj(`"key":{"type":"string"},"body":{"type":"string"}`, "key", "body"), Gated: true},
		{Name: "jira_transition", Description: "Apply a workflow transition (by transition id from jira_transitions) to an issue. GATED (changes issue status).",
			Parameters: obj(`"key":{"type":"string"},"transition_id":{"type":"string"}`, "key", "transition_id"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "jira_search":
		return d.search(ctx, call.Arguments)
	case "jira_get":
		return d.get(ctx, call.Arguments)
	case "jira_transitions":
		return d.transitions(ctx, call.Arguments)
	case "jira_create":
		return d.create(ctx, call.Arguments)
	case "jira_comment":
		return d.comment(ctx, call.Arguments)
	case "jira_transition":
		return d.transition(ctx, call.Arguments)
	default:
		return "unknown atlassian tool: " + call.Name, true, nil
	}
}

func unmarshalArgs(args json.RawMessage, v any) (string, bool) {
	if err := json.Unmarshal(args, v); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	return "", false
}

func (d *Dispatcher) search(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		JQL string `json:"jql"`
		Max int    `json:"max"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.JQL) == "" {
		return "jql is required", true, nil
	}
	issues, err := d.svc.Jira.Search(ctx, a.JQL, a.Max)
	if err != nil {
		return "jira search failed: " + err.Error(), true, nil
	}
	if len(issues) == 0 {
		return "no issues matched", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d issue(s):\n", len(issues))
	for _, is := range issues {
		fmt.Fprintf(&b, "- %s | %s | %s | %s\n", is.Key, is.Status, is.Type, is.Summary)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) get(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Key string `json:"key"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	is, err := d.svc.Jira.Get(ctx, a.Key)
	if err != nil {
		return "jira get failed: " + err.Error(), true, nil
	}
	assignee := is.Assignee
	if assignee == "" {
		assignee = "(unassigned)"
	}
	return fmt.Sprintf("%s [%s] %s\nType: %s · Assignee: %s\n\n%s",
		is.Key, is.Status, is.Summary, is.Type, assignee, is.Description), false, nil
}

func (d *Dispatcher) transitions(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Key string `json:"key"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	ts, err := d.svc.Jira.Transitions(ctx, a.Key)
	if err != nil {
		return "jira transitions failed: " + err.Error(), true, nil
	}
	if len(ts) == 0 {
		return "no transitions available on " + a.Key, false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "transitions for %s:\n", a.Key)
	for _, t := range ts {
		fmt.Fprintf(&b, "- id=%s | %s → %s\n", t.ID, t.Name, t.To)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) create(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Project     string `json:"project"`
		Type        string `json:"type"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Summary) == "" {
		return "summary is required", true, nil
	}
	detail := fmt.Sprintf("create issue %q in %s", a.Summary, firstNonEmptyStr(a.Project, d.svc.project, "(default)"))
	if err := d.policy.gate(ctx, classWrite, detail); err != nil {
		return err.Error(), true, nil
	}
	key, err := d.svc.Jira.Create(ctx, a.Project, a.Type, a.Summary, a.Description)
	if err != nil {
		return "jira create failed: " + err.Error(), true, nil
	}
	return "created issue " + key, false, nil
}

func (d *Dispatcher) comment(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Key  string `json:"key"`
		Body string `json:"body"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Key) == "" || strings.TrimSpace(a.Body) == "" {
		return "key and body are required", true, nil
	}
	if err := d.policy.gate(ctx, classWrite, "comment on "+a.Key); err != nil {
		return err.Error(), true, nil
	}
	if err := d.svc.Jira.Comment(ctx, a.Key, a.Body); err != nil {
		return "jira comment failed: " + err.Error(), true, nil
	}
	return "commented on " + a.Key, false, nil
}

func (d *Dispatcher) transition(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Key          string `json:"key"`
		TransitionID string `json:"transition_id"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Key) == "" || strings.TrimSpace(a.TransitionID) == "" {
		return "key and transition_id are required", true, nil
	}
	if err := d.policy.gate(ctx, classWrite, fmt.Sprintf("transition %s via %s", a.Key, a.TransitionID)); err != nil {
		return err.Error(), true, nil
	}
	if err := d.svc.Jira.Transition(ctx, a.Key, a.TransitionID); err != nil {
		return "jira transition failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("transitioned %s (transition %s)", a.Key, a.TransitionID), false, nil
}
