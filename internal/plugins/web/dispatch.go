package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/model"
)

// ActionClass is the blast-radius tier of a web action. The Policy
// gate keys off this — never off the tool name (a new tool must pick a
// class explicitly, so adding a tool can't silently bypass the gate).
type ActionClass int

const (
	ClassRead     ActionClass = iota // navigate, read, find, links, screenshot
	ClassInteract                    // click, fill, select (next turn)
	ClassTransact                    // submit, purchase, auth (gated, deny by default)
)

// Policy decides whether an action of a given class may proceed.
// Defaults are SAFE: read always; interact allowed; transact DENIED
// unless an explicit Confirm hook approves it. The model cannot change
// the policy — it's set by the operator at construction.
type Policy struct {
	AllowInteract bool
	// Confirm is consulted for every ClassTransact action. nil ⇒ deny
	// all transactions (the safe default). A CLI flag / human prompt /
	// allow-list wires a real implementation. Receives a human-readable
	// description of exactly what is about to happen.
	Confirm func(ctx context.Context, action, detail string) bool
}

// gate returns nil if the action may proceed, else an error explaining
// why it was blocked (fed back to the model as a tool error, so it
// learns the boundary instead of looping).
func (p Policy) gate(ctx context.Context, class ActionClass, action, detail string) error {
	switch class {
	case ClassRead:
		return nil
	case ClassInteract:
		if p.AllowInteract {
			return nil
		}
		// "web_interact" maps to the web safety category at the broker.
		if p.Confirm != nil && p.Confirm(ctx, "web_interact", action+": "+detail) {
			return nil
		}
		return fmt.Errorf("blocked: interaction (%s) is disabled by policy", action)
	case ClassTransact:
		// "web_transact" maps to destructive — never blanket-allowed by an
		// interact grant.
		if p.Confirm != nil && p.Confirm(ctx, "web_transact", action+": "+detail) {
			return nil
		}
		return fmt.Errorf("blocked: %s is an irreversible/financial action and was not confirmed "+
			"(transact actions require explicit approval — this is a safety boundary, not a bug)", action)
	default:
		return fmt.Errorf("blocked: unknown action class for %s", action)
	}
}

// Dispatcher implements agent.ToolDispatcher for the web plugin. It
// holds the browser session; tools operate on the current page.
type Dispatcher struct {
	br      browser
	policy  Policy
	shotDir string // where web_screenshot writes PNGs
	cfg     Config // carries search keys + reader-fallback config

	// reader-fallback state (TEN-113): lastURL is the page the agent is
	// "on" (set by navigate/click); lastText caches reader-sourced markdown
	// when the browser couldn't load that page itself. One Dispatcher per
	// agent and tool calls are serialized, so plain fields are safe.
	lastURL  string
	lastText string
}

// NewDispatcher wires a dispatcher over a session with a policy.
// shotDir is created lazily on first screenshot.
func NewDispatcher(s *Session, policy Policy, shotDir string) *Dispatcher {
	var (
		br  browser
		cfg Config
	)
	if s != nil { // nil session ⇒ spec-listing only (e.g. unconfigured stub)
		br = s.browser()
		cfg = s.Config()
	}
	return &Dispatcher{br: br, policy: policy, shotDir: shotDir, cfg: cfg}
}

// newDispatcherWithBrowser is the test seam (inject a fake browser).
func newDispatcherWithBrowser(b browser, policy Policy, shotDir string) *Dispatcher {
	return &Dispatcher{br: b, policy: policy, shotDir: shotDir}
}

// Tools returns the tool specs to register with agent.ToolRegistry.
// Only the read/explore layer ships this turn; interact/transact specs
// land alongside their implementations behind the same gate.
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
		{Name: "web_search", Description: "Search the live web for a query. Returns the top K results as a numbered list of {title, url, snippet}. Use this to DISCOVER URLs to read — then web_navigate into the most promising ones and web_read their content. Backed by DuckDuckGo by default; Tavily or Brave when an API key is configured.",
			Parameters: obj(`"query":{"type":"string"},"k":{"type":"integer","description":"number of results (default 8, max 25)"}`, "query")},
		{Name: "web_navigate", Description: "Load a URL in the browser and return the page title. Use before reading.",
			Parameters: obj(`"url":{"type":"string","description":"absolute http(s) URL"}`, "url")},
		{Name: "web_read", Description: "Return the visible text of the current page (what a user would see).",
			Parameters: obj(``)},
		{Name: "web_find", Description: "Find page text containing a query string. Returns matching snippets.",
			Parameters: obj(`"query":{"type":"string"}`, "query")},
		{Name: "web_links", Description: "List the links on the current page (text + href).",
			Parameters: obj(``)},
		{Name: "web_screenshot", Description: "Capture a screenshot of the current page; returns the file path.",
			Parameters: obj(``)},
		{Name: "web_click", Description: "Click an element. Locator = its visible text (preferred — e.g. \"Sign in\") or a CSS selector. Dangerous clicks (buy/checkout/pay/delete/login) require transact approval.",
			Parameters: obj(`"locator":{"type":"string","description":"visible text or CSS selector"}`, "locator"), Gated: true},
		{Name: "web_fill", Description: "Type a value into an input/textarea. Locator = its label/placeholder/name or a CSS selector. The value is never echoed back (privacy).",
			Parameters: obj(`"locator":{"type":"string"},"value":{"type":"string"}`, "locator", "value"), Gated: true},
		{Name: "web_select", Description: "Choose an option in a <select> dropdown by its visible label or value.",
			Parameters: obj(`"locator":{"type":"string"},"option":{"type":"string"}`, "locator", "option"), Gated: true},
	}
}

// dangerWords flag a click whose target text implies an irreversible /
// financial / auth action. Such a click is classified ClassTransact
// (denied unless Policy.Confirm approves) even though it arrived via
// web_click in the "interact" layer — the safety boundary is about
// what the button DOES, not which tool invoked it.
var dangerWords = []string{
	"buy", "purchase", "checkout", "check out", "pay", "payment",
	"place order", "order now", "confirm order", "complete purchase",
	"submit payment", "add card", "subscribe", "upgrade plan",
	"delete", "remove", "deactivate", "close account", "cancel subscription",
	"sign in", "signin", "log in", "login", "sign up", "signup",
	"authorize", "authorise", "grant access", "transfer", "send money",
}

func classifyClick(elemText string) (ActionClass, string) {
	t := strings.ToLower(strings.TrimSpace(elemText))
	for _, w := range dangerWords {
		if strings.Contains(t, w) {
			return ClassTransact, w
		}
	}
	return ClassInteract, ""
}

// Dispatch implements agent.ToolDispatcher: (result, isError, err).
// isError = a tool-level failure fed back to the model (bad args,
// policy block, page error). err = nil unless something truly
// unexpected; we prefer isError so the loop continues and the model
// can adjust.
func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "web_search":
		return HandleSearch(ctx, d.cfg, call.Arguments)
	case "web_navigate":
		return d.navigate(ctx, call.Arguments)
	case "web_read":
		return d.read(ctx)
	case "web_find":
		return d.find(ctx, call.Arguments)
	case "web_links":
		return d.links(ctx)
	case "web_screenshot":
		return d.screenshot(ctx)
	case "web_click":
		return d.click(ctx, call.Arguments)
	case "web_fill":
		return d.fill(ctx, call.Arguments)
	case "web_select":
		return d.selectOpt(ctx, call.Arguments)
	default:
		return "unknown web tool: " + call.Name, true, nil
	}
}

func (d *Dispatcher) click(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Locator string `json:"locator"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Locator) == "" {
		return "locator is required", true, nil
	}
	// Resolve FIRST (no side effect) so the gate decision is based on
	// the actual element the user would click.
	loc, err := d.br.Locate(ctx, a.Locator)
	if err != nil {
		return "locate failed: " + err.Error(), true, nil
	}
	if !loc.Found {
		return fmt.Sprintf("no clickable element matched %q (try its exact visible text, or a CSS selector)", a.Locator), true, nil
	}
	class, danger := classifyClick(loc.Text)
	detail := fmt.Sprintf("click %q (%s)", loc.Text, loc.Tag)
	if class == ClassTransact {
		detail = fmt.Sprintf("IRREVERSIBLE click %q — matched %q", loc.Text, danger)
	}
	if err := d.policy.gate(ctx, class, "web_click", detail); err != nil {
		return err.Error(), true, nil
	}
	newURL, err := d.br.Click(ctx, a.Locator)
	if err != nil {
		return "click failed: " + err.Error(), true, nil
	}
	if newURL == "" {
		return fmt.Sprintf("clicked %q (page did not navigate)", loc.Text), false, nil
	}
	d.lastURL, d.lastText = newURL, "" // followed a link; reader cache is stale
	return fmt.Sprintf("clicked %q — now at %s", loc.Text, newURL), false, nil
}

func (d *Dispatcher) fill(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Locator string `json:"locator"`
		Value   string `json:"value"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Locator) == "" {
		return "locator is required", true, nil
	}
	// Fill is interact-class. Gate detail NEVER includes the value —
	// it may be a password/PII; it must not leak into tool results,
	// the archive, or episodes ("User privacy" soul value).
	if err := d.policy.gate(ctx, ClassInteract, "web_fill", "fill "+a.Locator); err != nil {
		return err.Error(), true, nil
	}
	if err := d.br.Fill(ctx, a.Locator, a.Value); err != nil {
		return "fill failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("filled %q (%d chars)", a.Locator, len(a.Value)), false, nil
}

func (d *Dispatcher) selectOpt(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Locator string `json:"locator"`
		Option  string `json:"option"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Locator) == "" || strings.TrimSpace(a.Option) == "" {
		return "locator and option are required", true, nil
	}
	if err := d.policy.gate(ctx, ClassInteract, "web_select", fmt.Sprintf("select %q in %s", a.Option, a.Locator)); err != nil {
		return err.Error(), true, nil
	}
	if err := d.br.Select(ctx, a.Locator, a.Option); err != nil {
		return "select failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("selected %q in %q", a.Option, a.Locator), false, nil
}

func (d *Dispatcher) navigate(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	u, err := validateURL(a.URL)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := d.policy.gate(ctx, ClassRead, "navigate", u); err != nil {
		return err.Error(), true, nil
	}
	d.lastURL, d.lastText = u, "" // remember target; drop any stale reader cache
	title, err := d.br.Navigate(ctx, u)
	if err == nil {
		return fmt.Sprintf("loaded %q\ntitle: %s", u, title), false, nil
	}
	// Browser failed (timeout / blocked / network): fall back to the reader,
	// which renders server-side and returns markdown. web_read then serves the
	// cached reader text for this page.
	if !d.cfg.ReaderDisabled {
		if txt, rerr := fetchViaReader(ctx, d.cfg, u); rerr == nil && strings.TrimSpace(txt) != "" {
			d.lastText = txt
			return fmt.Sprintf("loaded %q via reader fallback (the browser was blocked or timed out) — call web_read to read it.", u), false, nil
		}
	}
	return "navigation failed: " + err.Error(), true, nil
}

func (d *Dispatcher) read(ctx context.Context) (string, bool, error) {
	if err := d.policy.gate(ctx, ClassRead, "read", ""); err != nil {
		return err.Error(), true, nil
	}
	// Reader-sourced text from a fallback navigate wins — the browser never
	// actually loaded this page.
	if strings.TrimSpace(d.lastText) != "" {
		return capText(d.lastText), false, nil
	}
	txt, err := d.br.Text(ctx)
	if err == nil && !looksBlocked(txt) {
		return capText(txt), false, nil
	}
	// Empty text, a bot-wall interstitial, or a browser error: try the reader
	// fallback on the current page.
	if !d.cfg.ReaderDisabled {
		target := d.lastURL
		if target == "" {
			target, _ = d.br.CurrentURL(ctx)
		}
		if target != "" {
			if rtxt, rerr := fetchViaReader(ctx, d.cfg, target); rerr == nil && strings.TrimSpace(rtxt) != "" {
				d.lastText = rtxt
				return capText(rtxt), false, nil
			}
		}
	}
	if err != nil {
		return "read failed: " + err.Error(), true, nil
	}
	if strings.TrimSpace(txt) == "" {
		return "(page has no visible text — try web_screenshot, or the page may not have loaded)", false, nil
	}
	return capText(txt), false, nil // non-empty but looked blocked; reader didn't help — return what we have
}

// capText bounds a page's text so one huge page can't blow the agent's context
// budget (~8k chars ≈ ~2k tokens). The assembler also budgets, but bounding here
// keeps the tool result itself sane.
func capText(s string) string {
	const limit = 8000
	s = strings.TrimSpace(s)
	if len(s) > limit {
		return s[:limit] + "\n…[truncated; use web_find to locate specifics]"
	}
	return s
}

func (d *Dispatcher) find(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return "query is required", true, nil
	}
	if err := d.policy.gate(ctx, ClassRead, "find", a.Query); err != nil {
		return err.Error(), true, nil
	}
	matches, err := d.br.Find(ctx, a.Query)
	if err != nil {
		return "find failed: " + err.Error(), true, nil
	}
	if len(matches) == 0 {
		return fmt.Sprintf("no matches for %q on this page", a.Query), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q:\n", len(matches), a.Query)
	for i, m := range matches {
		fmt.Fprintf(&b, "%d. %s\n", i+1, m)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) links(ctx context.Context) (string, bool, error) {
	if err := d.policy.gate(ctx, ClassRead, "links", ""); err != nil {
		return err.Error(), true, nil
	}
	links, err := d.br.Links(ctx)
	if err != nil {
		return "links failed: " + err.Error(), true, nil
	}
	if len(links) == 0 {
		return "no links on this page", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d link(s):\n", len(links))
	for i, l := range links {
		t := l.Text
		if t == "" {
			t = "(no text)"
		}
		fmt.Fprintf(&b, "%d. %s -> %s\n", i+1, t, l.Href)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) screenshot(ctx context.Context) (string, bool, error) {
	if err := d.policy.gate(ctx, ClassRead, "screenshot", ""); err != nil {
		return err.Error(), true, nil
	}
	cur, _ := d.br.CurrentURL(ctx)
	if err := os.MkdirAll(d.shotDir, 0o755); err != nil {
		return "screenshot dir error: " + err.Error(), true, nil
	}
	path := filepath.Join(d.shotDir, fmt.Sprintf("web-%d.png", time.Now().UnixNano()))
	if err := d.br.Screenshot(ctx, path); err != nil {
		return "screenshot failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("screenshot of %s saved to %s", cur, path), false, nil
}

// validateURL enforces http/https + an absolute URL. Blocks file://,
// javascript:, data:, and bare hosts — the model navigating to a
// local file:// or a javascript: URL is an injection/exfil risk.
func validateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("blocked: only http/https URLs allowed (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("blocked: url must include a host")
	}
	return u.String(), nil
}
