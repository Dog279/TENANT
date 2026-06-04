// Package imessage is Tenant's iMessage connector via a BlueBubbles
// server. There is no official Apple API; BlueBubbles is the
// maintained open-source bridge that runs on a Mac with Messages and
// exposes a password-authed REST API (it owns the chat.db /
// AppleScript / Private-API plumbing). Tenant treats it as a networked
// dependency, like vLLM/Ollama/Chrome — a thin native-Go REST client,
// no SDK.
package imessage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpDoer is the seam every BlueBubbles call goes through (tests
// inject an httptest server instead of a real Mac).
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config opens a Service against a BlueBubbles server.
type Config struct {
	URL        string // e.g. http://localhost:1234 (the BlueBubbles server)
	Password   string // BlueBubbles server password
	PrivateAPI bool   // use the "private-api" send method (else "apple-script")
	HTTP       httpDoer
}

// Service is the opened iMessage/BlueBubbles connector.
type Service struct {
	a          *api
	sendMethod string
}

// Open validates config. No network is touched here (lazy); the first
// call hits the server.
func Open(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("imessage: BlueBubbles server URL required (--bb-url or $BLUEBUBBLES_URL)")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("imessage: invalid BlueBubbles URL %q", cfg.URL)
	}
	if strings.TrimSpace(cfg.Password) == "" {
		return nil, fmt.Errorf("imessage: BlueBubbles password required (--bb-password or $BLUEBUBBLES_PASSWORD)")
	}
	h := cfg.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	method := "apple-script"
	if cfg.PrivateAPI {
		method = "private-api"
	}
	return &Service{
		a:          &api{http: h, base: strings.TrimRight(u.String(), "/"), password: cfg.Password},
		sendMethod: method,
	}, nil
}

// api is the authed transport. BlueBubbles auth is a password query
// param on every request; responses are wrapped in a
// {status,message,data} envelope.
type api struct {
	http     httpDoer
	base     string
	password string
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("BlueBubbles error (HTTP %d): %s", e.Status, e.Msg)
}

// do issues a request to /api/v1<path>. query holds extra params
// (password is always added). The decoded envelope.data is unmarshalled
// into out.
func (a *api) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("password", a.password)
	full := a.base + "/api/v1" + path + "?" + query.Encode()

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("imessage: request: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var env struct {
		Status  int             `json:"status"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(data, &env)
	if resp.StatusCode/100 != 2 {
		msg := env.Message
		if msg == "" {
			msg = clip(string(data), 300)
		}
		return &apiError{Status: resp.StatusCode, Msg: msg}
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// Ping checks connectivity to the BlueBubbles server.
func (s *Service) Ping(ctx context.Context) error {
	return s.a.do(ctx, "GET", "/ping", nil, nil, nil)
}

// tempGUID is a client-generated id BlueBubbles uses to dedupe an
// apple-script send before the real message guid exists.
func tempGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "tenant-" + hex.EncodeToString(b)
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// msToTime formats a BlueBubbles millisecond epoch into a readable
// timestamp ("" when absent).
func msToTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}
