package x

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const apiBase = "https://api.twitter.com/2"

// Config opens a Service. Bearer enables read-only (app-only). A PKCE
// token store (from `tenant x --login`) enables user-context reads +
// posting. At least one must be usable.
type Config struct {
	Bearer    string // app-only bearer token (reads)
	TokenPath string // PKCE token store path (user context: reads + posts)
	AllowPost bool   // selects least-privilege scopes at login time

	HTTP  httpDoer         // nil ⇒ http.DefaultClient
	Clock func() time.Time // nil ⇒ time.Now (tests inject)
}

// Service is the opened X connector. read serves GET endpoints (bearer
// if available, else the user token); user serves posting and is nil
// when only a bearer is configured.
type Service struct {
	read *api
	user *api // nil ⇒ no user context (cannot post)
}

func Open(cfg Config) (*Service, error) {
	h := cfg.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	var userTS tokenSource
	if cfg.TokenPath != "" {
		if _, err := os.Stat(cfg.TokenPath); err == nil {
			p, perr := newPKCESource(cfg.TokenPath, h, cfg.Clock)
			if perr != nil {
				return nil, perr
			}
			userTS = p
		}
	}
	var readTS tokenSource
	switch {
	case cfg.Bearer != "":
		readTS = bearerSource{cfg.Bearer}
	case userTS != nil:
		readTS = userTS // user token can also read
	default:
		return nil, fmt.Errorf("x: no credentials — set --bearer (read) or run `tenant x --login` (post)")
	}
	svc := &Service{read: &api{http: h, ts: readTS}}
	if userTS != nil {
		svc.user = &api{http: h, ts: userTS}
	}
	return svc, nil
}

// canPost reports whether a user-context token is available.
func (s *Service) canPost() bool { return s.user != nil }

// api is the authenticated JSON transport every X call shares.
type api struct {
	http httpDoer
	ts   tokenSource
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("X API error (HTTP %d): %s", e.Status, e.Msg)
}

func (a *api) do(ctx context.Context, method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	tok, err := a.ts.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("x: request: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return &apiError{Status: resp.StatusCode, Msg: xErrMsg(data)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// xErrMsg pulls the human message out of X's error envelope (it uses
// a few shapes: top-level detail/title, or an errors[] array).
func xErrMsg(b []byte) string {
	var e struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &e) == nil {
		switch {
		case e.Detail != "":
			return e.Detail
		case len(e.Errors) > 0 && e.Errors[0].Message != "":
			return e.Errors[0].Message
		case e.Title != "":
			return e.Title
		}
	}
	s := string(b)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
