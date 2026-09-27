// Package simplefin is Tenant's read-only personal-finance connector,
// speaking the SimpleFIN protocol (simplefin.org) against a bridge the
// operator pays for and links banks through (bridge.simplefin.org).
//
// Design ethos (matches wiki: the simple correct thing, readable
// top-to-bottom):
//
//   - READ-ONLY BY PROTOCOL. The bridge holds the bank connections; the
//     only credential here is one access URL. Nothing in this package
//     can move money or see bank passwords — so no safety gate.
//
//   - The append-only ledger (ledger.go) is the canonical local record:
//     one JSONL file you can cat. Money is int64 CENTS (never floats),
//     days are America/Los_Angeles dates. These constraints are shared
//     with the wealthd design so its brain can later ingest this exact
//     log.
//
//   - Stdlib only: net/http + encoding/json + time/tzdata. No modules.
package simplefin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // LA dates must work on hosts without an OS tz database (Windows)
)

// httpDoer is the transport seam (mirrors gsuite): nil ⇒ real network.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// maxBody caps any bridge response read (accounts JSON for a personal
// book is a few hundred KB at most; 8MB is a generous ceiling).
const maxBody = 8 << 20

// Config opens a Service over one SimpleFIN access URL — the value a
// one-time Claim of the operator's setup token returned. The URL embeds
// basic-auth credentials (https://user:pass@bridge/simplefin).
type Config struct {
	AccessURL string
	HTTP      httpDoer         // nil ⇒ http.DefaultClient
	Clock     func() time.Time // nil ⇒ time.Now (tests inject)
}

// Service is the opened connector: base endpoint + auth, no state.
type Service struct {
	base  string // access URL with the userinfo stripped, no trailing slash
	user  string
	pass  string
	http  httpDoer
	clock func() time.Time
}

// Open validates the access URL and prepares the client. No network.
func Open(cfg Config) (*Service, error) {
	raw := strings.TrimSpace(cfg.AccessURL)
	if raw == "" {
		return nil, fmt.Errorf("simplefin: access URL required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("simplefin: parse access URL: %w", err)
	}
	if err := requireHTTPS(u); err != nil {
		return nil, err
	}
	if u.User == nil {
		return nil, fmt.Errorf("simplefin: access URL must embed credentials (https://user:pass@bridge/…) — it is the value the claim returned, not the bridge homepage")
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	base := *u
	base.User = nil
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Transport: Events.Observe(nil)} // bridge failures reach the plugin's log
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		base:  strings.TrimRight(base.String(), "/"),
		user:  user,
		pass:  pass,
		http:  h,
		clock: clock,
	}, nil
}

// requireHTTPS rejects plaintext transport for real hosts. Loopback is
// allowed so httptest servers (and a local dev bridge) work.
func requireHTTPS(u *url.URL) error {
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return fmt.Errorf("simplefin: refusing non-https URL %q", u.Scheme+"://"+u.Host)
}

// --- setup-token claim ------------------------------------------------

// LooksLikeSetupToken reports whether s is a one-time SimpleFIN setup
// token (base64 of a claim URL) rather than an already-claimed access
// URL. Lets every entry point accept either form.
func LooksLikeSetupToken(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "://") {
		return false
	}
	claim, err := decodeSetupToken(s)
	return err == nil && strings.Contains(claim, "://")
}

// decodeSetupToken tolerates every base64 variant (bridges emit standard
// padding; docs sometimes strip it; URL-safe copies survive too).
func decodeSetupToken(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", fmt.Errorf("simplefin: setup token is not valid base64")
}

// Claim redeems a one-time setup token for the long-lived access URL.
// ONE SHOT by protocol: a second claim of the same token fails at the
// bridge (it treats reuse as compromise), so the caller must persist the
// returned access URL immediately.
func Claim(ctx context.Context, setupToken string, h httpDoer) (string, error) {
	claimURL, err := decodeSetupToken(setupToken)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(claimURL)
	if err != nil {
		return "", fmt.Errorf("simplefin: setup token decoded to an unparseable URL: %w", err)
	}
	if err := requireHTTPS(u); err != nil {
		return "", err
	}
	if h == nil {
		h = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, strings.NewReader(""))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Length", "0")
	resp, err := h.Do(req)
	if err != nil {
		return "", fmt.Errorf("simplefin: claim: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("simplefin: claim failed (HTTP %d): %s — setup tokens are one-time; if this one was ever used or leaked, generate a fresh token at the bridge", resp.StatusCode, clipStr(string(body), 200))
	}
	access := strings.TrimSpace(string(body))
	if _, err := Open(Config{AccessURL: access}); err != nil {
		return "", fmt.Errorf("simplefin: claim returned an invalid access URL: %w", err)
	}
	return access, nil
}

// --- accounts fetch ---------------------------------------------------

// AccountSet is the bridge's /accounts response. Errors carries
// per-institution notices ("Connection to X needs attention") — data may
// still be present alongside them.
type AccountSet struct {
	Errors   []string  `json:"errors"`
	Accounts []Account `json:"accounts"`
}

// Account mirrors the SimpleFIN wire shape. NOTE the protocol's mixed
// naming: account fields are hyphenated (balance-date), transaction
// fields use underscores (transacted_at). Amounts are decimal STRINGS —
// ParseCents converts.
type Account struct {
	Org              OrgInfo       `json:"org"`
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Currency         string        `json:"currency"`
	Balance          string        `json:"balance"`
	AvailableBalance string        `json:"available-balance"`
	BalanceDate      int64         `json:"balance-date"` // unix seconds
	Transactions     []Transaction `json:"transactions"`
	Holdings         []Holding     `json:"holdings"`
}

type OrgInfo struct {
	Domain  string `json:"domain"`
	Name    string `json:"name"`
	SfinURL string `json:"sfin-url"`
}

type Transaction struct {
	ID           string `json:"id"`
	Posted       int64  `json:"posted"` // unix seconds
	Amount       string `json:"amount"` // negative = money out
	Description  string `json:"description"`
	Payee        string `json:"payee"`
	Memo         string `json:"memo"`
	Pending      bool   `json:"pending"`
	TransactedAt int64  `json:"transacted_at"`
}

// Holding is an investment position. Parsed best-effort — bridges vary;
// absent fields stay zero and the account-level balance is authoritative.
type Holding struct {
	Description string `json:"description"`
	Symbol      string `json:"symbol"`
	Shares      string `json:"shares"`
	MarketValue string `json:"market_value"`
	CostBasis   string `json:"cost_basis"`
}

// Accounts fetches balances + transactions posted in [start, end],
// pending included.
func (s *Service) Accounts(ctx context.Context, start, end time.Time) (*AccountSet, error) {
	q := url.Values{}
	q.Set("start-date", strconv.FormatInt(start.Unix(), 10))
	q.Set("end-date", strconv.FormatInt(end.Unix(), 10))
	q.Set("pending", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/accounts?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.user, s.pass)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("simplefin: accounts: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("simplefin: read accounts: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("simplefin: accounts HTTP 403 — the bridge revoked this access URL (it flags reuse of a claimed token as compromise); generate a NEW setup token at the bridge and reconfigure")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("simplefin: accounts HTTP %d: %s", resp.StatusCode, clipStr(string(body), 200))
	}
	var set AccountSet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("simplefin: decode accounts: %w", err)
	}
	return &set, nil
}

// --- money as cents ---------------------------------------------------

// ParseCents converts a SimpleFIN decimal string ("-4.50", "1234", "5.1")
// to int64 cents. Floats are never involved. More than two fraction
// digits is an error unless the extra digits are zero (some bridges pad).
func ParseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("simplefin: empty amount")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(frac) > 2 {
		if strings.Trim(frac[2:], "0") != "" {
			return 0, fmt.Errorf("simplefin: amount %q has sub-cent precision", s)
		}
		frac = frac[:2]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("simplefin: bad amount %q", s)
	}
	cents64, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("simplefin: bad amount fraction %q", s)
	}
	v := whole*100 + cents64
	if neg {
		v = -v
	}
	return v, nil
}

// FormatCents renders cents for humans/models: USD gets "-$4.50", other
// currencies "-4.50 EUR".
func FormatCents(c int64, currency string) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	body := fmt.Sprintf("%d.%02d", c/100, c%100)
	if currency == "" || strings.EqualFold(currency, "USD") {
		return sign + "$" + body
	}
	return sign + body + " " + currency
}

// --- LA dates ---------------------------------------------------------

var (
	laOnce sync.Once
	laLoc  *time.Location
)

// LA returns America/Los_Angeles (embedded tzdata guarantees success on
// Windows; the fixed-zone fallback is belt-and-braces).
func LA() *time.Location {
	laOnce.Do(func() {
		l, err := time.LoadLocation("America/Los_Angeles")
		if err != nil {
			l = time.FixedZone("PT", -8*3600)
		}
		laLoc = l
	})
	return laLoc
}

// Day renders a unix timestamp as its LA calendar date.
func Day(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).In(LA()).Format("2006-01-02")
}

func clipStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
