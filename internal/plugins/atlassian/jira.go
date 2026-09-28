package atlassian

// jira.go wraps go-atlassian's v2 Issue services into the small, flat surface
// the agent tools need (TEN-52): JQL search, get, create, comment, list/apply
// transitions. v2 (not v3/ADF) keeps descriptions/comments as plain strings —
// the v3+ADF migration is the deferred TEN-61.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	v2 "github.com/ctreminiom/go-atlassian/v2/jira/v2"
	model "github.com/ctreminiom/go-atlassian/v2/pkg/infra/models"
)

// JiraClient is the wrapped go-atlassian client + the default project key.
type JiraClient struct {
	c       *v2.Client
	project string
}

// Issue is the flat, render-ready view of a Jira issue.
type Issue struct {
	Key         string
	Summary     string
	Status      string
	Type        string
	Assignee    string
	Description string
}

// Transition is one available workflow transition for an issue.
type Transition struct {
	ID   string
	Name string
	To   string
}

func toIssue(is *model.IssueSchemeV2) Issue {
	out := Issue{Key: is.Key}
	if f := is.Fields; f != nil {
		out.Summary = f.Summary
		out.Description = f.Description
		if f.Status != nil {
			out.Status = f.Status.Name
		}
		if f.IssueType != nil {
			out.Type = f.IssueType.Name
		}
		if f.Assignee != nil {
			out.Assignee = f.Assignee.DisplayName
		}
	}
	return out
}

const issueFields = "summary,status,issuetype,assignee,description"

// Search runs a JQL query and returns up to max issues (default 25, cap 50)
// and the token for the next page ("" on the last). It uses the enhanced
// search endpoint: Atlassian removed GET /rest/api/2/search in 2025 (TEN-60).
func (j *JiraClient) Search(ctx context.Context, jql string, max int, pageToken string) ([]Issue, string, error) {
	if max <= 0 || max > 50 {
		max = 25
	}
	res, resp, err := j.c.Issue.Search.SearchJQL(ctx, jql, []string{"summary", "status", "issuetype", "assignee"}, nil, max, pageToken)
	if err != nil {
		return nil, "", apiErr(resp, err)
	}
	out := make([]Issue, 0, len(res.Issues))
	for _, is := range res.Issues {
		out = append(out, toIssue(is))
	}
	return out, res.NextPageToken, nil
}

// apiErr adds Atlassian's own reason to err. go-atlassian maps every non-2xx
// to a bare sentinel ("bad request"), dropping the body's errorMessages and a
// 429's Retry-After, which an unattended agent needs to recover (TEN-60).
func apiErr(resp *model.ResponseScheme, err error) error {
	if err == nil || resp == nil || resp.Response == nil {
		return err
	}
	if resp.Code == http.StatusTooManyRequests {
		if after := resp.Header.Get("Retry-After"); after != "" {
			return fmt.Errorf("rate limited by Atlassian (HTTP 429): retry after %s seconds", after)
		}
		return fmt.Errorf("rate limited by Atlassian (HTTP 429): retry later")
	}
	var body struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	_ = json.Unmarshal(resp.Bytes.Bytes(), &body)
	msgs := body.ErrorMessages
	for _, k := range slices.Sorted(maps.Keys(body.Errors)) {
		msgs = append(msgs, k+": "+body.Errors[k])
	}
	if len(msgs) == 0 {
		return fmt.Errorf("%w (HTTP %d)", err, resp.Code)
	}
	return fmt.Errorf("%w (HTTP %d): %s", err, resp.Code, strings.Join(msgs, "; "))
}

// Get returns one issue by key or id.
func (j *JiraClient) Get(ctx context.Context, keyOrID string) (Issue, error) {
	is, resp, err := j.c.Issue.Get(ctx, keyOrID, []string{"summary", "status", "issuetype", "assignee", "description"}, nil)
	if err != nil {
		return Issue{}, apiErr(resp, err)
	}
	return toIssue(is), nil
}

// Create makes a new issue. project falls back to the client's default.
func (j *JiraClient) Create(ctx context.Context, project, issueType, summary, description string) (string, error) {
	if project == "" {
		project = j.project
	}
	if project == "" {
		return "", fmt.Errorf("atlassian: create needs a project key (none given and no default configured)")
	}
	if issueType == "" {
		issueType = "Task"
	}
	payload := &model.IssueSchemeV2{Fields: &model.IssueFieldsSchemeV2{
		Summary:     summary,
		Description: description,
		Project:     &model.ProjectScheme{Key: project},
		IssueType:   &model.IssueTypeScheme{Name: issueType},
	}}
	created, resp, err := j.c.Issue.Create(ctx, payload, nil)
	if err != nil {
		return "", apiErr(resp, err)
	}
	return created.Key, nil
}

// Comment adds a plain-text comment to an issue.
func (j *JiraClient) Comment(ctx context.Context, keyOrID, body string) error {
	_, resp, err := j.c.Issue.Comment.Add(ctx, keyOrID, &model.CommentPayloadSchemeV2{Body: body}, nil)
	return apiErr(resp, err)
}

// Transitions lists the workflow transitions currently available on an issue.
func (j *JiraClient) Transitions(ctx context.Context, keyOrID string) ([]Transition, error) {
	res, resp, err := j.c.Issue.Transitions(ctx, keyOrID)
	if err != nil {
		return nil, apiErr(resp, err)
	}
	out := make([]Transition, 0, len(res.Transitions))
	for _, t := range res.Transitions {
		tr := Transition{ID: t.ID, Name: t.Name}
		if t.To != nil {
			tr.To = t.To.Name
		}
		out = append(out, tr)
	}
	return out, nil
}

// Transition applies a workflow transition (by transition id) to an issue.
func (j *JiraClient) Transition(ctx context.Context, keyOrID, transitionID string) error {
	resp, err := j.c.Issue.Move(ctx, keyOrID, transitionID, nil)
	return apiErr(resp, err)
}
