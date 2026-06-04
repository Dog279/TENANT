// Package web is Tenant's web-utility plugin: a stateful headless
// Chrome session the agent drives across turns to read, explore, and
// (later) interact with the live web.
//
// Layered by blast radius:
//
//	read/explore  navigate, read, find, links, screenshot   — always allowed
//	interact      click, fill, select                       — allowed (reversible-ish)
//	transact      submit, purchase, auth                     — confirm-gated, DENY by default
//
// Why a real browser, not HTTP fetch: "act as a normal user" means
// JS-rendered pages, SPAs, cookie/session state — exactly what a bare
// HTTP GET cannot see. Chrome renders the page the way the user does.
//
// Chrome is an EXTERNAL runtime (like vLLM / Ollama). The Tenant
// binary stays pure-Go single-binary; chromedp drives Chrome over the
// DevTools Protocol. No CGO.
//
// The browser interface decouples the agent-facing Dispatcher from
// chromedp so policy/gate/routing logic is unit-testable with a fake
// (real Chrome is exercised via `tenant web` integration runs — the
// same fakes-for-logic / real-runs-for-IO split used for vLLM/doctor).
package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// stealthJS runs before every page's own scripts (via
// Page.addScriptToEvaluateOnNewDocument) to erase the headless-automation
// tells bot-detectors probe: navigator.webdriver, an empty plugins/
// languages list, and the missing window.chrome object. This is the
// minimal subset of what puppeteer-extra-stealth / undetected-chromedriver
// do — enough to clear the instant-block layer, NOT a defeat for
// Cloudflare/DataDome/Google, which also weigh behavior and IP reputation.
const stealthJS = `
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
window.chrome = window.chrome || {runtime: {}};
// Only FILL IN languages/plugins when empty (legacy headless). Modern
// Chrome already ships a real PluginArray + languages; clobbering them
// with plain arrays REGRESSES the fingerprint (a live sannysoft run
// showed our fake [1..5] failing the "is of type PluginArray" check that
// the real one passes).
if (!navigator.languages || navigator.languages.length === 0) {
  Object.defineProperty(navigator, 'languages', {get: () => ['en-US', 'en']});
}
if (!navigator.plugins || navigator.plugins.length === 0) {
  Object.defineProperty(navigator, 'plugins', {get: () => [1, 2, 3, 4, 5]});
}
const _q = navigator.permissions && navigator.permissions.query;
if (_q) {
  navigator.permissions.query = (p) =>
    p && p.name === 'notifications'
      ? Promise.resolve({state: Notification.permission})
      : _q(p);
}
`

// applyStealth runs once at session start (already connected to Chrome).
// It (1) overrides the UA + sec-ch-ua client hints from the REAL running
// version so they're self-consistent, and (2) installs stealthJS to run
// before every document. Both are best-effort: a CDP failure here must
// not kill the browser (the real Chrome UA is a fine fallback), so we
// swallow errors and return nil. The session is already proven up by the
// time this runs — if Chrome hadn't launched, chromedp.Run would have
// failed before reaching this ActionFunc.
func applyStealth(ctx context.Context) error {
	if _, product, _, ua, _, err := cdpbrowser.GetVersion().Do(ctx); err == nil {
		cleanUA, meta := consistentUA(ua, product)
		// Clean tags: setUserAgentOverride.acceptLanguage ALSO populates
		// navigator.languages (parsed literally), so a q-weighted value
		// would leak a malformed "en;q=0.9" tag — the JS-visible array is
		// what detectors read, so keep it clean.
		_ = emulation.SetUserAgentOverride(cleanUA).
			WithAcceptLanguage("en-US,en").
			WithPlatform(meta.Platform).
			WithUserAgentMetadata(meta).
			Do(ctx)
	}
	_, _ = page.AddScriptToEvaluateOnNewDocument(stealthJS).Do(ctx)
	return nil
}

// blockedExtensions are subresource file types dropped network-wide for the
// whole session: heavy media + fonts that waste bandwidth/CPU and are never
// needed to extract a page's text (a real win when concurrent research
// subagents each drive their own Chrome). Images and CSS are deliberately KEPT
// — web_screenshot needs images, and innerText extraction relies on CSS
// visibility. Host-based ad/tracker blocking is deferred: it no longer affects
// timeouts now that Navigate waits for DOMContentLoaded rather than `load`.
var blockedExtensions = []string{
	".mp4", ".webm", ".ogg", ".ogv", ".mp3", ".wav", ".m4a", ".avi", ".mov",
	".woff", ".woff2", ".ttf", ".otf", ".eot",
}

// blockResources enables the Network domain and blocks blockedExtensions via
// URLPattern rules (CDP Network.setBlockedURLs; pattern syntax per the protocol
// example "*://*:*/*.css"). Best-effort: a CDP hiccup here must not kill an
// otherwise-good session, so errors are swallowed.
func blockResources(ctx context.Context) error {
	if err := network.Enable().Do(ctx); err != nil {
		return nil
	}
	pats := make([]*network.BlockPattern, 0, len(blockedExtensions))
	for _, ext := range blockedExtensions {
		pats = append(pats, &network.BlockPattern{URLPattern: "*://*:*/*" + ext, Block: true})
	}
	_ = network.SetBlockedURLs().WithURLPatterns(pats).Do(ctx)
	return nil
}

var chromeVerRe = regexp.MustCompile(`(?:Headless)?Chrome/(\d+)(\.[\d.]+)?`)

// consistentUA derives a self-consistent UA string + UA-CH metadata from
// the live browser's reported UA/product. It strips the "HeadlessChrome"
// token (a dead giveaway) and builds sec-ch-ua brands whose versions
// match the SAME major — the mismatch detectors flag is exactly when the
// UA major and the client-hint major disagree.
func consistentUA(rawUA, product string) (string, *emulation.UserAgentMetadata) {
	src := rawUA
	if src == "" {
		src = product
	}
	major, full := "131", "131.0.0.0"
	if m := chromeVerRe.FindStringSubmatch(src); m != nil {
		major = m[1]
		full = major + m[2]
		if m[2] == "" {
			full = major + ".0.0.0"
		}
	}
	ua := rawUA
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/" + full + " Safari/537.36"
	}
	// HeadlessChrome → Chrome so the UA token doesn't betray the session.
	ua = chromeVerRe.ReplaceAllString(ua, "Chrome/"+full)

	plat, platVer := "Windows", "10.0.0"
	switch runtime.GOOS {
	case "darwin":
		plat, platVer = "macOS", "14.0.0"
	case "linux":
		plat, platVer = "Linux", ""
	}
	// The "Not_A Brand" GREASE entry mirrors what real Chrome sends.
	brands := []*emulation.UserAgentBrandVersion{
		{Brand: "Not_A Brand", Version: "24"},
		{Brand: "Chromium", Version: major},
		{Brand: "Google Chrome", Version: major},
	}
	fullList := []*emulation.UserAgentBrandVersion{
		{Brand: "Not_A Brand", Version: "24.0.0.0"},
		{Brand: "Chromium", Version: full},
		{Brand: "Google Chrome", Version: full},
	}
	meta := &emulation.UserAgentMetadata{
		Brands:          brands,
		FullVersionList: fullList,
		Platform:        plat,
		PlatformVersion: platVer,
		Architecture:    "x86",
		Bitness:         "64",
		Model:           "",
		Mobile:          false,
	}
	return ua, meta
}

// Link is one anchor discovered on a page.
type Link struct {
	Text string `json:"text"`
	Href string `json:"href"`
}

// LocateResult describes an element the locator resolved to, WITHOUT
// acting on it. The Dispatcher inspects Text/Tag to classify a click's
// blast radius (a "Buy Now" button is transact, not interact) before
// the gate runs — so the danger decision is based on the element the
// user would actually be clicking.
type LocateResult struct {
	Found bool   `json:"found"`
	Tag   string `json:"tag"`  // a | button | input | select | textarea
	Type  string `json:"type"` // for input: text|password|email|submit|...
	Text  string `json:"text"` // visible text / value / aria-label / placeholder
}

// browser is the minimal surface the Dispatcher needs. cdpBrowser is
// the chromedp implementation; tests use a fake.
type browser interface {
	Navigate(ctx context.Context, url string) (title string, err error)
	Text(ctx context.Context) (string, error)
	Links(ctx context.Context) ([]Link, error)
	Find(ctx context.Context, query string) ([]string, error)
	Screenshot(ctx context.Context, path string) error
	CurrentURL(ctx context.Context) (string, error)
	// interact layer:
	Locate(ctx context.Context, locator string) (LocateResult, error)
	Click(ctx context.Context, locator string) (newURL string, err error)
	Fill(ctx context.Context, locator, value string) error
	Select(ctx context.Context, locator, option string) error
	Close() error
}

// Config configures a browser session AND the web_search backend (search has
// no browser dependency, so its options ride here for one config object).
type Config struct {
	// ChromePath overrides Chrome auto-detection.
	ChromePath string
	// Headless runs Chrome without a window (default true). Set false
	// to watch the agent drive a visible browser (useful for trust /
	// debugging interact+transact later).
	Headless bool
	// NavTimeout bounds a single navigation. Default 60s — modern news /
	// finance pages (Yahoo Finance, MarketWatch, Fool, etc.) push multiple
	// MB of JS + trackers and routinely take 20-40s to fire the load event,
	// and concurrent research subagents each driving their own headless
	// Chrome amplify that. 30s left too many "context deadline exceeded"
	// failures on the exact sites researchers pick first.
	NavTimeout time.Duration
	// BraveKey, when set, switches web_search to the Brave Search API
	// (cleaner results, predictable rate limits). Empty → DuckDuckGo (the
	// always-on, keyless default). NOTE: Brave dropped its free tier in 2025
	// (now metered) — prefer TavilyKey, which has a real free tier.
	BraveKey string
	// TavilyKey switches web_search to the Tavily API — LLM-ready results
	// (title+url+content) with a real free tier. Search backend precedence:
	// Tavily > Brave > DuckDuckGo (keyless default).
	TavilyKey string
	// JinaKey authenticates the r.jina.ai reader fallback for a higher rate
	// limit. Empty = keyless (still works, lower limit).
	JinaKey string
	// ReaderDisabled turns OFF the Jina reader fallback entirely (operator
	// opt-out: no URL is ever sent to r.jina.ai). Default false = enabled.
	ReaderDisabled bool
}

// Session owns a Chrome process + a single tab. One Session per agent
// conversation; the Dispatcher holds it and tools operate on the
// current page. Close tears Chrome down.
type Session struct {
	cfg      Config
	allocCtx context.Context
	cancelA  context.CancelFunc
	tabCtx   context.Context
	cancelT  context.CancelFunc
}

// Config returns the session's config — used by Dispatcher to surface
// BraveKey to the (browser-less) web_search tool.
func (s *Session) Config() Config { return s.cfg }

// detectChrome finds a Chrome/Chromium/Edge binary. Explicit path
// wins; otherwise probes the well-known OS locations.
// DetectChrome locates a Chrome / Chromium / Edge binary on the host.
// Exported so `tenant doctor` can pre-flight the web plugin without
// importing the rest of the session machinery.
func DetectChrome(explicit string) (string, error) { return detectChrome(explicit) }

func detectChrome(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("web: chrome not at %s", explicit)
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default: // linux
		candidates = []string{
			"/usr/bin/google-chrome", "/usr/bin/chromium",
			"/usr/bin/chromium-browser", "/snap/bin/chromium",
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("web: no Chrome/Chromium/Edge found (set Config.ChromePath)")
}

// NewSession starts Chrome and opens one tab.
func NewSession(cfg Config) (*Session, error) {
	chromePath, err := detectChrome(cfg.ChromePath)
	if err != nil {
		return nil, err
	}
	if cfg.NavTimeout == 0 {
		cfg.NavTimeout = 60 * time.Second
	}
	// Headless is the caller's choice (TUI passes !--web-show). The old
	// code forced it true unconditionally, so --web-show never opened a
	// window.
	headless := cfg.Headless

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		// Hide the automation tells bot-detectors check FIRST: the
		// AutomationControlled blink feature and the enable-automation
		// switch ("Chrome is being controlled by automated software").
		// Without these you're flagged before any page script runs.
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
		// Clean tags only — this populates navigator.languages, which must
		// NOT contain q-weights ("en;q=0.9" is a malformed tag = a tell).
		// The q-weighted Accept-Language HEADER is set via CDP in
		// applyStealth instead.
		chromedp.Flag("accept-lang", "en-US,en"),
	)
	// UA is set via CDP after launch (applyStealth) so the UA string and
	// the sec-ch-ua client hints are derived from the SAME real version —
	// no static-UA-vs-client-hint mismatch.
	// DefaultExecAllocatorOptions sets --headless=true; override it. "new"
	// headless renders like full Chrome and is far harder to fingerprint
	// than the legacy headless shell.
	if headless {
		opts = append(opts, chromedp.Flag("headless", "new"))
	} else {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	allocCtx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	// Filter chromedp's noisy "unhandled node event *dom.EventXxx" stream —
	// newer Chrome versions emit DOM events (EventAdRelatedStateUpdated,
	// EventScrollableFlagUpdated, etc.) that cdproto doesn't recognize yet,
	// and chromedp logs every one at ERROR by default. Hundreds per page on
	// commercial sites; it floods the TUI feed and stderr. Real errors
	// (anything not "unhandled node event") still propagate through.
	tabCtx, cancelT := chromedp.NewContext(allocCtx, chromedp.WithErrorf(filteredErrorf))
	// Force the browser to actually start now so failures surface here,
	// not on the first tool call mid-conversation. The stealth script is
	// installed to run on EVERY new document (before the page's own JS),
	// so navigator.webdriver et al. are patched before detectors read them.
	if err := chromedp.Run(tabCtx,
		chromedp.ActionFunc(applyStealth),
		chromedp.ActionFunc(blockResources),
	); err != nil {
		cancelT()
		cancelA()
		return nil, fmt.Errorf("web: start chrome: %w", err)
	}
	return &Session{
		cfg: cfg, allocCtx: allocCtx, cancelA: cancelA,
		tabCtx: tabCtx, cancelT: cancelT,
	}, nil
}

// Close shuts Chrome down. Idempotent.
func (s *Session) Close() error {
	if s.cancelT != nil {
		s.cancelT()
		s.cancelT = nil
	}
	if s.cancelA != nil {
		s.cancelA()
		s.cancelA = nil
	}
	return nil
}

// --- cdpBrowser: chromedp implementation of browser ---

type cdpBrowser struct {
	tabCtx  context.Context
	navTime time.Duration
}

func (s *Session) browser() browser {
	return &cdpBrowser{tabCtx: s.tabCtx, navTime: s.cfg.NavTimeout}
}

// navSettle bounds how long we wait for the `load` event before giving up on it
// and proceeding with whatever DOM is ready. Modern news/finance pages keep
// firing analytics + lazy ads long after the page is readable, so `load` never
// settles — but DOMContentLoaded (body present) fired long ago. Capped to the
// overall NavTimeout.
const navSettle = 12 * time.Second

// Navigate loads url and returns the title. It retries ONE transient/fast
// failure (a slow timeout is never retried — that would just double the wait).
func (b *cdpBrowser) Navigate(ctx context.Context, url string) (string, error) {
	var (
		title string
		err   error
	)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(700 * time.Millisecond): // brief backoff
			case <-ctx.Done():
				return "", err
			}
		}
		title, err = b.navigateOnce(ctx, url)
		if err == nil || ctx.Err() != nil || isDeadlineErr(err) {
			return title, err
		}
	}
	return "", err
}

// navigateOnce issues the navigation but does NOT block on the `load` event:
// it waits for `load` only up to navSettle, then proceeds once the body exists
// (≈ DOMContentLoaded) and reads the title — "page is usable" without requiring
// every subresource to finish. This is the per-call shape the live sannysoft
// test already uses by hand.
func (b *cdpBrowser) navigateOnce(ctx context.Context, url string) (string, error) {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()

	settle := navSettle
	if b.navTime < settle {
		settle = b.navTime
	}
	navCtx, navCancel := context.WithTimeout(rctx, settle)
	nerr := chromedp.Run(navCtx, chromedp.Navigate(url))
	navCancel()
	// A real navigation error (bad DNS, connection refused) surfaces fast and
	// is NOT just a slow `load` — fail now so it can be retried.
	if nerr != nil && !isDeadlineErr(nerr) && rctx.Err() == nil {
		return "", fmt.Errorf("web: navigate %s: %w", url, nerr)
	}

	var title string
	werr := chromedp.Run(rctx,
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Title(&title),
	)
	if werr == nil {
		return title, nil
	}
	// Last-ditch soft success: the title may be readable even if WaitReady
	// timed out waiting for a never-ready body.
	if isDeadlineErr(werr) {
		rec, rcancel := context.WithTimeout(b.tabCtx, 5*time.Second)
		defer rcancel()
		if terr := chromedp.Run(rec, chromedp.Title(&title)); terr == nil && strings.TrimSpace(title) != "" {
			return title, nil
		}
	}
	return "", fmt.Errorf("web: navigate %s: %w", url, werr)
}

// isDeadlineErr reports whether err is (or wraps) a context deadline — chromedp
// sometimes returns the string form rather than the sentinel.
func isDeadlineErr(err error) bool {
	return err != nil && (errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "deadline exceeded"))
}

func (b *cdpBrowser) Text(ctx context.Context) (string, error) {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var txt string
	// innerText (not textContent) ≈ what a user sees: skips hidden
	// nodes, collapses whitespace.
	if err := chromedp.Run(rctx,
		chromedp.Evaluate(`document.body ? document.body.innerText : ""`, &txt),
	); err != nil {
		return "", fmt.Errorf("web: read text: %w", err)
	}
	return txt, nil
}

func (b *cdpBrowser) Links(ctx context.Context) ([]Link, error) {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var links []Link
	js := `[...document.querySelectorAll('a[href]')].slice(0,200).map(a=>({text:(a.innerText||'').trim().slice(0,120),href:a.href}))`
	if err := chromedp.Run(rctx, chromedp.Evaluate(js, &links)); err != nil {
		return nil, fmt.Errorf("web: links: %w", err)
	}
	return links, nil
}

func (b *cdpBrowser) Find(ctx context.Context, query string) ([]string, error) {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var matches []string
	// Case-insensitive innerText contains; return the matched
	// elements' trimmed text (capped) so the model can locate things.
	js := fmt.Sprintf(`(()=>{const q=%q.toLowerCase();
	  return [...document.querySelectorAll('body *')]
	    .filter(e=>e.children.length===0 && e.innerText && e.innerText.toLowerCase().includes(q))
	    .slice(0,30).map(e=>e.innerText.trim().slice(0,200));})()`, query)
	if err := chromedp.Run(rctx, chromedp.Evaluate(js, &matches)); err != nil {
		return nil, fmt.Errorf("web: find %q: %w", query, err)
	}
	return matches, nil
}

func (b *cdpBrowser) Screenshot(ctx context.Context, path string) error {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var buf []byte
	if err := chromedp.Run(rctx, chromedp.FullScreenshot(&buf, 80)); err != nil {
		return fmt.Errorf("web: screenshot: %w", err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return fmt.Errorf("web: write screenshot %s: %w", path, err)
	}
	return nil
}

func (b *cdpBrowser) CurrentURL(ctx context.Context) (string, error) {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var u string
	if err := chromedp.Run(rctx, chromedp.Location(&u)); err != nil {
		return "", fmt.Errorf("web: current url: %w", err)
	}
	return u, nil
}

// jsResolve is the shared element resolver: try the locator as a CSS
// selector first, then fall back to matching visible text/value/
// aria-label/placeholder among interactive elements (how an LLM
// "sees" the page via web_read). Returns the element handle setter
// into window.__tnEl so Click/Fill/Select act on the SAME element
// Locate inspected.
const jsResolve = `(loc => {
  let el=null;
  try { el=document.querySelector(loc); } catch(e){}
  if(!el){
    const c=[...document.querySelectorAll('a,button,input,select,textarea,[role=button],[onclick]')];
    const L=String(loc).trim().toLowerCase();
    const t=e=>((e.innerText||e.value||e.getAttribute&&e.getAttribute('aria-label')||e.placeholder||'').trim().toLowerCase());
    el = c.find(e=>t(e)===L) || c.find(e=>t(e).includes(L));
  }
  window.__tnEl = el;
  if(!el) return {found:false};
  return {found:true, tag:el.tagName.toLowerCase(), type:(el.type||''),
    text:((el.innerText||el.value||(el.getAttribute&&el.getAttribute('aria-label'))||el.placeholder||'').trim().slice(0,160))};
})`

func (b *cdpBrowser) Locate(ctx context.Context, locator string) (LocateResult, error) {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var res LocateResult
	js := fmt.Sprintf("(%s)(%q)", jsResolve, locator)
	if err := chromedp.Run(rctx, chromedp.Evaluate(js, &res)); err != nil {
		return LocateResult{}, fmt.Errorf("web: locate %q: %w", locator, err)
	}
	return res, nil
}

func (b *cdpBrowser) Click(ctx context.Context, locator string) (string, error) {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var ok struct {
		Found bool `json:"found"`
	}
	clickJS := fmt.Sprintf(`(()=>{(%s)(%q);
	  if(!window.__tnEl) return {found:false};
	  window.__tnEl.scrollIntoView({block:'center'});
	  window.__tnEl.click();
	  return {found:true};})()`, jsResolve, locator)
	if err := chromedp.Run(rctx, chromedp.Evaluate(clickJS, &ok)); err != nil {
		return "", fmt.Errorf("web: click %q: %w", locator, err)
	}
	if !ok.Found {
		return "", fmt.Errorf("web: click: no element matched %q", locator)
	}
	// Let any navigation / AJAX settle, then report where we are.
	var u string
	_ = chromedp.Run(rctx,
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Location(&u),
	)
	return u, nil
}

func (b *cdpBrowser) Fill(ctx context.Context, locator, value string) error {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var ok struct {
		Found bool `json:"found"`
	}
	// Set value + dispatch input & change so React/Vue/Angular pick it
	// up (raw .value assignment alone is invisible to most SPAs).
	js := fmt.Sprintf(`(()=>{(%s)(%q);
	  const el=window.__tnEl;
	  if(!el) return {found:false};
	  el.focus();
	  el.value=%q;
	  el.dispatchEvent(new Event('input',{bubbles:true}));
	  el.dispatchEvent(new Event('change',{bubbles:true}));
	  return {found:true};})()`, jsResolve, locator, value)
	if err := chromedp.Run(rctx, chromedp.Evaluate(js, &ok)); err != nil {
		return fmt.Errorf("web: fill %q: %w", locator, err)
	}
	if !ok.Found {
		return fmt.Errorf("web: fill: no input matched %q", locator)
	}
	return nil
}

func (b *cdpBrowser) Select(ctx context.Context, locator, option string) error {
	rctx, cancel := context.WithTimeout(mergeCtx(ctx, b.tabCtx), b.navTime)
	defer cancel()
	var r struct {
		Found bool `json:"found"`
		Set   bool `json:"set"`
	}
	// Match an <option> by value OR visible label (LLMs supply the
	// label they read, not the value attribute).
	js := fmt.Sprintf(`(()=>{(%s)(%q);
	  const el=window.__tnEl;
	  if(!el||el.tagName.toLowerCase()!=='select') return {found:false};
	  const want=%q.trim().toLowerCase();
	  let set=false;
	  for(const o of el.options){
	    if(o.value.toLowerCase()===want || o.text.trim().toLowerCase()===want){
	      el.value=o.value; set=true; break;
	    }
	  }
	  if(set) el.dispatchEvent(new Event('change',{bubbles:true}));
	  return {found:true, set:set};})()`, jsResolve, locator, option)
	if err := chromedp.Run(rctx, chromedp.Evaluate(js, &r)); err != nil {
		return fmt.Errorf("web: select %q: %w", locator, err)
	}
	if !r.Found {
		return fmt.Errorf("web: select: %q is not a <select> element", locator)
	}
	if !r.Set {
		return fmt.Errorf("web: select: no option %q in %q", option, locator)
	}
	return nil
}

func (b *cdpBrowser) Close() error { return nil } // session owns Chrome lifecycle

// mergeCtx returns a context cancelled when EITHER the caller's ctx or
// filteredErrorf is chromedp's error logger with the "unhandled node event"
// floodgate closed. cdproto trails Chrome's protocol surface (new DOM event
// types ship in every Chrome release before cdproto adds them), so chromedp
// emits ERROR for each unknown event — hundreds per page on commercial
// sites. Everything else still goes to stderr unchanged.
func filteredErrorf(format string, args ...any) {
	if strings.Contains(format, "unhandled") && strings.Contains(format, "event") {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// the chrome tab ctx is done. chromedp.Run must run on the tab ctx;
// we still want the agent's cancellation to abort a slow page.
func mergeCtx(caller, tab context.Context) context.Context {
	// chromedp requires its own context lineage, so run on tab ctx but
	// propagate caller cancellation via a watcher.
	merged, cancel := context.WithCancel(tab)
	go func() {
		select {
		case <-caller.Done():
			cancel()
		case <-merged.Done():
		}
	}()
	return merged
}
