// Package discord is Tenant's Discord connector — a CGO-free pure-Go
// REST client against the official Discord API (https://discord.com/api/v10)
// authenticated as a bot. Surface A: the agent can SEND, READ, REACT, and
// LIST channels/guilds. Surface B (Discord users → agent.Turn via the Gateway
// WebSocket) is implemented in gateway.go (TEN-115): the Gateway lifecycle
// (opcodes 7/9, resume_gateway_url, identify backoff) drives a dedicated agent
// turn from an @mention/DM, gated by the relay's operator allowlist and the
// per-action button approver.
//
// Implementation notes:
//   - No SDK. Direct net/http calls; the RetryDecorator already shipped
//     today handles transient 429/5xx retries within the bounded
//     allowlist (web_*/sql_*/embed). Mutating tools (send, react) are
//     added to the deny list in internal/agent/retry.go.
//   - Token storage: credentials.json (0600) via Tenant's existing
//     skill:discord:token secret path (see cmd/tenant/skills_setup.go).
//   - Rate-limit policy: respect Retry-After on 429. No proactive bucket
//     tracking (per-route X-RateLimit-Bucket is complex and the bot is
//     unlikely to push 50 req/sec from a sole-operator deployment).
//   - allowed_mentions default: parse=[] — neither @everyone, role
//     pings, nor user mentions resolve unless the operator overrides
//     per-message. This matches Hermes's safety default and prevents
//     accidental mass-pings from a hallucinating model.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL is the Discord REST API v10 base. v10 is current as of
// 2026-05-25 per discord.com/developers/reference. Override via Config
// for tests against httptest.
const defaultBaseURL = "https://discord.com/api/v10"

// userAgent — Discord requires a recognizable User-Agent on bot
// requests. Format per discord.com/developers/reference: "DiscordBot
// (<url>, <version>)". Tenant identifies itself so a future Discord
// abuse-report or rate-limit incident can be traced to a Tenant build.
const userAgent = "DiscordBot (https://github.com/tenant-mcp/tenant, 1.0)"

// httpDoer is the seam every Discord call goes through (tests inject
// an httptest server instead of api.discord.com).
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config opens a Service against the Discord REST API.
type Config struct {
	Token   string   // bot token (without the "Bot " prefix; we add it)
	BaseURL string   // override for tests; empty → defaultBaseURL
	HTTP    httpDoer // override for tests; nil → http.DefaultClient
}

// Service is the opened Discord connector.
type Service struct {
	a *api
}

// Open validates config. No network is touched here (lazy); the first
// REST call hits Discord.
func Open(cfg Config) (*Service, error) {
	tok := strings.TrimSpace(cfg.Token)
	if tok == "" {
		return nil, fmt.Errorf("discord: bot token required (--discord-bot-token or $DISCORD_BOT_TOKEN)")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	h := cfg.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	return &Service{a: &api{http: h, base: strings.TrimRight(base, "/"), token: tok}}, nil
}

// api is the authed REST transport.
type api struct {
	http  httpDoer
	base  string
	token string
}

// apiError carries Discord's structured error response shape. Discord
// returns {"code": N, "message": "..."} on most failures; some 4xx
// responses also include {"errors": {...}} with per-field validation.
type apiError struct {
	Status     int           // HTTP status
	DiscordErr int           // Discord's numeric error code (50001 = missing access, etc.)
	Message    string        // human-readable message
	RetryAfter time.Duration // populated on 429
}

func (e *apiError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("discord %d: %s (rate-limited; retry after %s)", e.Status, e.Message, e.RetryAfter)
	}
	if e.DiscordErr != 0 {
		return fmt.Sprintf("discord %d (code %d): %s", e.Status, e.DiscordErr, e.Message)
	}
	return fmt.Sprintf("discord %d: %s", e.Status, e.Message)
}

// do executes a request and decodes the response into out (may be nil
// for fire-and-forget endpoints like PUT reaction). On 2xx returns
// nil; on 4xx/5xx returns *apiError so callers can inspect Status +
// DiscordErr. On 429 sets RetryAfter from Retry-After header.
func (a *api) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := a.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var bodyR io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("discord: marshal body: %w", err)
		}
		bodyR = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyR)
	if err != nil {
		return fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+a.token)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("discord: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(rawBody) == 0 {
			return nil
		}
		if err := json.Unmarshal(rawBody, out); err != nil {
			return fmt.Errorf("discord: decode response: %w", err)
		}
		return nil
	}
	// Error path: parse Discord's structured error if present.
	ae := &apiError{Status: resp.StatusCode}
	var dErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rawBody, &dErr)
	ae.DiscordErr = dErr.Code
	ae.Message = dErr.Message
	if ae.Message == "" {
		ae.Message = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.ParseFloat(ra, 64); err == nil {
				ae.RetryAfter = time.Duration(secs * float64(time.Second))
			}
		}
	}
	return ae
}
