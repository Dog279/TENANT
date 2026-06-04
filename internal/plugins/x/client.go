package x

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Tweet is a normalized post.
type Tweet struct {
	ID, Text, AuthorID, AuthorHandle, CreatedAt string
	Likes, Retweets, Replies                    int
}

// User is a normalized profile.
type User struct {
	ID, Username, Name, Description string
	Followers, Following, Tweets    int
	Verified                        bool
}

// --- reads (svc.read) ---

func (s *Service) SearchRecent(ctx context.Context, query string, max int) ([]Tweet, error) {
	if max <= 0 {
		max = 10
	}
	if max < 10 {
		max = 10 // X search/recent minimum
	}
	if max > 100 {
		max = 100
	}
	q := url.Values{
		"query":        {query},
		"max_results":  {fmt.Sprintf("%d", max)},
		"tweet.fields": {"created_at,public_metrics,author_id"},
		"expansions":   {"author_id"},
		"user.fields":  {"username"},
	}
	var r tweetsResp
	if err := s.read.do(ctx, "GET", apiBase+"/tweets/search/recent?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	return r.normalize(), nil
}

func (s *Service) GetTweet(ctx context.Context, id string) (*Tweet, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("x: tweet id required")
	}
	q := url.Values{
		"tweet.fields": {"created_at,public_metrics,author_id,conversation_id"},
		"expansions":   {"author_id"},
		"user.fields":  {"username"},
	}
	var r struct {
		Data     tweetData `json:"data"`
		Includes includes  `json:"includes"`
	}
	if err := s.read.do(ctx, "GET", apiBase+"/tweets/"+url.PathEscape(id)+"?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	t := r.Data.to(r.Includes.handles())
	return &t, nil
}

func (s *Service) GetUser(ctx context.Context, username string) (*User, error) {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return nil, fmt.Errorf("x: username required")
	}
	q := url.Values{"user.fields": {"description,public_metrics,verified"}}
	var r struct {
		Data userData `json:"data"`
	}
	if err := s.read.do(ctx, "GET", apiBase+"/users/by/username/"+url.PathEscape(username)+"?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	if r.Data.ID == "" {
		return nil, fmt.Errorf("x: user @%s not found", username)
	}
	u := r.Data.to()
	return &u, nil
}

func (s *Service) UserTimeline(ctx context.Context, username string, max int) ([]Tweet, error) {
	u, err := s.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 10
	}
	if max < 5 {
		max = 5 // X users/:id/tweets minimum
	}
	if max > 100 {
		max = 100
	}
	q := url.Values{
		"max_results":  {fmt.Sprintf("%d", max)},
		"tweet.fields": {"created_at,public_metrics,author_id"},
	}
	var r tweetsResp
	if err := s.read.do(ctx, "GET", apiBase+"/users/"+url.PathEscape(u.ID)+"/tweets?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := r.normalize()
	for i := range out {
		out[i].AuthorHandle = u.Username
	}
	return out, nil
}

// --- writes (svc.user; nil ⇒ no user context) ---

func (s *Service) PostTweet(ctx context.Context, text string) (*Tweet, error) {
	return s.post(ctx, map[string]any{"text": text}, text)
}

func (s *Service) Reply(ctx context.Context, text, inReplyTo string) (*Tweet, error) {
	if strings.TrimSpace(inReplyTo) == "" {
		return nil, fmt.Errorf("x: reply needs the tweet id to reply to")
	}
	return s.post(ctx, map[string]any{
		"text":  text,
		"reply": map[string]string{"in_reply_to_tweet_id": inReplyTo},
	}, text)
}

func (s *Service) post(ctx context.Context, body map[string]any, text string) (*Tweet, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("x: tweet text is empty")
	}
	if len([]rune(text)) > 280 {
		return nil, fmt.Errorf("x: tweet is %d chars (max 280)", len([]rune(text)))
	}
	if s.user == nil {
		return nil, fmt.Errorf("x: posting needs user context — run `tenant x --login` (a bearer token cannot post)")
	}
	var r struct {
		Data tweetData `json:"data"`
	}
	if err := s.user.do(ctx, "POST", apiBase+"/tweets", body, &r); err != nil {
		return nil, err
	}
	t := r.Data.to(nil)
	return &t, nil
}

func (s *Service) DeleteTweet(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("x: tweet id required")
	}
	if s.user == nil {
		return false, fmt.Errorf("x: deleting needs user context — run `tenant x --login`")
	}
	var r struct {
		Data struct {
			Deleted bool `json:"deleted"`
		} `json:"data"`
	}
	if err := s.user.do(ctx, "DELETE", apiBase+"/tweets/"+url.PathEscape(id), nil, &r); err != nil {
		return false, err
	}
	return r.Data.Deleted, nil
}

// --- X API v2 payload shapes (only what we use) ---

type tweetData struct {
	ID            string `json:"id"`
	Text          string `json:"text"`
	AuthorID      string `json:"author_id"`
	CreatedAt     string `json:"created_at"`
	PublicMetrics struct {
		Like    int `json:"like_count"`
		Retweet int `json:"retweet_count"`
		Reply   int `json:"reply_count"`
	} `json:"public_metrics"`
}

func (d tweetData) to(handles map[string]string) Tweet {
	return Tweet{
		ID: d.ID, Text: d.Text, AuthorID: d.AuthorID,
		AuthorHandle: handles[d.AuthorID], CreatedAt: d.CreatedAt,
		Likes: d.PublicMetrics.Like, Retweets: d.PublicMetrics.Retweet,
		Replies: d.PublicMetrics.Reply,
	}
}

type userData struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Verified      bool   `json:"verified"`
	PublicMetrics struct {
		Followers int `json:"followers_count"`
		Following int `json:"following_count"`
		Tweet     int `json:"tweet_count"`
	} `json:"public_metrics"`
}

func (u userData) to() User {
	return User{
		ID: u.ID, Username: u.Username, Name: u.Name, Description: u.Description,
		Followers: u.PublicMetrics.Followers, Following: u.PublicMetrics.Following,
		Tweets: u.PublicMetrics.Tweet, Verified: u.Verified,
	}
}

type includes struct {
	Users []userData `json:"users"`
}

func (i includes) handles() map[string]string {
	m := map[string]string{}
	for _, u := range i.Users {
		m[u.ID] = u.Username
	}
	return m
}

type tweetsResp struct {
	Data     []tweetData `json:"data"`
	Includes includes    `json:"includes"`
}

func (r tweetsResp) normalize() []Tweet {
	h := r.Includes.handles()
	out := make([]Tweet, 0, len(r.Data))
	for _, d := range r.Data {
		out = append(out, d.to(h))
	}
	return out
}
