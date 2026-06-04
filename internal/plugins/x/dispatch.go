package x

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// Policy gates X actions by blast radius — same shape as sql/web/
// gsuite. Read (search/lookup/timeline) is always allowed. Post
// (x_post, x_delete) is PUBLIC and effectively irreversible, so it is
// denied unless AllowPost, or a per-action Confirm explicitly
// approves it (nil ⇒ deny). The model cannot change the policy.
type Policy struct {
	AllowPost bool
	Confirm   func(ctx context.Context, action, detail string) bool
}

type actionClass int

const (
	classRead actionClass = iota
	classPost
)

func (p Policy) gate(ctx context.Context, c actionClass, detail string) error {
	if c == classRead {
		return nil
	}
	if p.AllowPost {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, "x_post", detail) {
		return nil
	}
	return fmt.Errorf("blocked: this posts/deletes publicly as the user and was not approved " +
		"— enable the post flag or confirm. This is a blast-radius boundary, not a bug")
}

// Dispatcher implements agent.ToolDispatcher for X.
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
		{Name: "x_search", Description: "Search recent (last ~7d) public tweets. X query syntax, e.g. `from:nasa -is:retweet`. Returns id/text/author/metrics.",
			Parameters: obj(`"query":{"type":"string"},"max":{"type":"integer","description":"10-100 (default 10)"}`, "query")},
		{Name: "x_get_tweet", Description: "Get one tweet by id (text, author handle, like/retweet/reply counts).",
			Parameters: obj(`"id":{"type":"string"}`, "id")},
		{Name: "x_get_user", Description: "Look up a user by @username (bio, follower/following/tweet counts, verified).",
			Parameters: obj(`"username":{"type":"string"}`, "username")},
		{Name: "x_user_timeline", Description: "Recent tweets from a user's timeline by @username.",
			Parameters: obj(`"username":{"type":"string"},"max":{"type":"integer","description":"5-100 (default 10)"}`, "username")},
		{Name: "x_post", Description: "Post a tweet (≤280 chars), optionally as a reply. GATED: requires operator approval (it is public + as the user).",
			Parameters: obj(`"text":{"type":"string"},"reply_to":{"type":"string","description":"tweet id to reply to (optional)"}`, "text"), Gated: true},
		{Name: "x_delete", Description: "Delete one of the user's own tweets by id. GATED: requires operator approval (irreversible).",
			Parameters: obj(`"id":{"type":"string"}`, "id"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "x_search":
		return d.search(ctx, call.Arguments)
	case "x_get_tweet":
		return d.getTweet(ctx, call.Arguments)
	case "x_get_user":
		return d.getUser(ctx, call.Arguments)
	case "x_user_timeline":
		return d.timeline(ctx, call.Arguments)
	case "x_post":
		return d.post(ctx, call.Arguments)
	case "x_delete":
		return d.del(ctx, call.Arguments)
	default:
		return "unknown x tool: " + call.Name, true, nil
	}
}

func unmarshal(args json.RawMessage, v any) (string, bool) {
	if err := json.Unmarshal(args, v); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	return "", false
}

func fmtTweet(t Tweet) string {
	who := t.AuthorHandle
	if who == "" {
		who = t.AuthorID
	}
	return fmt.Sprintf("- id=%s @%s (%s) ♥%d ↻%d 💬%d\n  %s",
		t.ID, who, t.CreatedAt, t.Likes, t.Retweets, t.Replies,
		strings.ReplaceAll(t.Text, "\n", " "))
}

func (d *Dispatcher) search(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Query string `json:"query"`
		Max   int    `json:"max"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return "query is required", true, nil
	}
	ts, err := d.svc.SearchRecent(ctx, a.Query, a.Max)
	if err != nil {
		return "x search failed: " + err.Error(), true, nil
	}
	if len(ts) == 0 {
		return fmt.Sprintf("no recent tweets matched %q", a.Query), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d tweet(s):\n", len(ts))
	for _, t := range ts {
		b.WriteString(fmtTweet(t) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) getTweet(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	t, err := d.svc.GetTweet(ctx, a.ID)
	if err != nil {
		return "x get_tweet failed: " + err.Error(), true, nil
	}
	return fmtTweet(*t), false, nil
}

func (d *Dispatcher) getUser(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Username string `json:"username"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	u, err := d.svc.GetUser(ctx, a.Username)
	if err != nil {
		return "x get_user failed: " + err.Error(), true, nil
	}
	v := ""
	if u.Verified {
		v = " ✓"
	}
	return fmt.Sprintf("@%s%s — %s\nfollowers=%d following=%d tweets=%d\n%s",
		u.Username, v, u.Name, u.Followers, u.Following, u.Tweets, u.Description), false, nil
}

func (d *Dispatcher) timeline(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Username string `json:"username"`
		Max      int    `json:"max"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	ts, err := d.svc.UserTimeline(ctx, a.Username, a.Max)
	if err != nil {
		return "x user_timeline failed: " + err.Error(), true, nil
	}
	if len(ts) == 0 {
		return fmt.Sprintf("@%s has no recent tweets", strings.TrimPrefix(a.Username, "@")), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d tweet(s) from @%s:\n", len(ts), strings.TrimPrefix(a.Username, "@"))
	for _, t := range ts {
		b.WriteString(fmtTweet(t) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) post(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Text    string `json:"text"`
		ReplyTo string `json:"reply_to"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	kind := "tweet"
	if a.ReplyTo != "" {
		kind = "reply to " + a.ReplyTo
	}
	detail := fmt.Sprintf("%s: %q (%d chars)", kind, a.Text, len([]rune(a.Text)))
	if err := d.policy.gate(ctx, classPost, detail); err != nil {
		return err.Error(), true, nil
	}
	var (
		t   *Tweet
		err error
	)
	if a.ReplyTo != "" {
		t, err = d.svc.Reply(ctx, a.Text, a.ReplyTo)
	} else {
		t, err = d.svc.PostTweet(ctx, a.Text)
	}
	if err != nil {
		return "x post failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("posted: tweet id %s", t.ID), false, nil
}

func (d *Dispatcher) del(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	if err := d.policy.gate(ctx, classPost, "delete tweet "+a.ID); err != nil {
		return err.Error(), true, nil
	}
	ok, err := d.svc.DeleteTweet(ctx, a.ID)
	if err != nil {
		return "x delete failed: " + err.Error(), true, nil
	}
	if !ok {
		return "x delete: API reported not deleted", true, nil
	}
	return "deleted tweet " + a.ID, false, nil
}
