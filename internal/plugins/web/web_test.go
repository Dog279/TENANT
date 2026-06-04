package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
)

// fakeBrowser implements the unexported browser interface so the
// Dispatcher's routing + policy + arg handling are fully unit-tested
// without a real Chrome. Real Chrome is exercised via `tenant web`.
type fakeBrowser struct {
	navURL    string
	navTitle  string
	navErr    error
	text      string
	links     []Link
	findRes   []string
	shotErr   error
	shotCount int
	cur       string

	// interact layer
	locRes      LocateResult
	locErr      error
	lastLocator string
	clicked     string
	clickURL    string
	clickErr    error
	filledLoc   string
	filledVal   string
	fillErr     error
	selLoc      string
	selOpt      string
	selErr      error
}

func (f *fakeBrowser) Navigate(_ context.Context, u string) (string, error) {
	f.navURL = u
	return f.navTitle, f.navErr
}
func (f *fakeBrowser) Text(context.Context) (string, error)        { return f.text, nil }
func (f *fakeBrowser) Links(context.Context) ([]Link, error)       { return f.links, nil }
func (f *fakeBrowser) Find(_ context.Context, q string) ([]string, error) {
	return f.findRes, nil
}
func (f *fakeBrowser) Screenshot(_ context.Context, _ string) error { f.shotCount++; return f.shotErr }
func (f *fakeBrowser) CurrentURL(context.Context) (string, error)   { return f.cur, nil }
func (f *fakeBrowser) Close() error                                 { return nil }

// interact-layer fake fields/methods
func (f *fakeBrowser) Locate(_ context.Context, loc string) (LocateResult, error) {
	f.lastLocator = loc
	return f.locRes, f.locErr
}
func (f *fakeBrowser) Click(_ context.Context, loc string) (string, error) {
	f.clicked = loc
	return f.clickURL, f.clickErr
}
func (f *fakeBrowser) Fill(_ context.Context, loc, val string) error {
	f.filledLoc, f.filledVal = loc, val
	return f.fillErr
}
func (f *fakeBrowser) Select(_ context.Context, loc, opt string) error {
	f.selLoc, f.selOpt = loc, opt
	return f.selErr
}

func disp(fb *fakeBrowser, p Policy) *Dispatcher {
	// shotDir unused by the fake (Screenshot is stubbed), so "." is fine.
	return newDispatcherWithBrowser(fb, p, ".")
}

func call(name string, args map[string]any) model.ToolCall {
	b, _ := json.Marshal(args)
	return model.ToolCall{Name: name, Arguments: b}
}

// --- read/explore routing ---

func TestNavigate_OK(t2 *testing.T) {
	fb := &fakeBrowser{navTitle: "Example Domain"}
	d := disp(fb, Policy{})
	out, isErr, err := d.Dispatch(context.Background(), call("web_navigate", map[string]any{"url": "https://example.com/"}))
	if err != nil || isErr {
		t2.Fatalf("unexpected: isErr=%v err=%v out=%q", isErr, err, out)
	}
	if fb.navURL != "https://example.com/" {
		t2.Errorf("navigated to %q", fb.navURL)
	}
	if !strings.Contains(out, "Example Domain") {
		t2.Errorf("title not surfaced: %q", out)
	}
}

func TestNavigate_RejectsNonHTTP(t2 *testing.T) {
	d := disp(&fakeBrowser{}, Policy{})
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "data:text/html,x", "ftp://x", "notaurl"} {
		out, isErr, _ := d.Dispatch(context.Background(), call("web_navigate", map[string]any{"url": bad}))
		if !isErr {
			t2.Errorf("%q should be rejected, got %q", bad, out)
		}
	}
}

func TestNavigate_BadArgs(t2 *testing.T) {
	d := disp(&fakeBrowser{}, Policy{})
	out, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{Name: "web_navigate", Arguments: json.RawMessage(`{bad`)})
	if !isErr || !strings.Contains(out, "invalid arguments") {
		t2.Fatalf("want invalid-args error, got isErr=%v out=%q", isErr, out)
	}
}

func TestRead_CapsHugePage(t2 *testing.T) {
	fb := &fakeBrowser{text: strings.Repeat("x", 20000)}
	d := disp(fb, Policy{})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_read", nil))
	if isErr {
		t2.Fatal("read should not error")
	}
	if len(out) > 8200 || !strings.Contains(out, "truncated") {
		t2.Errorf("expected truncation, len=%d", len(out))
	}
}

func TestRead_EmptyPage(t2 *testing.T) {
	d := disp(&fakeBrowser{text: "   "}, Policy{})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_read", nil))
	if isErr || !strings.Contains(out, "no visible text") {
		t2.Fatalf("want graceful empty msg, got isErr=%v %q", isErr, out)
	}
}

func TestFind_MatchesAndMisses(t2 *testing.T) {
	fb := &fakeBrowser{findRes: []string{"the price is $42", "buy now"}}
	d := disp(fb, Policy{})
	out, _, _ := d.Dispatch(context.Background(), call("web_find", map[string]any{"query": "price"}))
	if !strings.Contains(out, "2 match") || !strings.Contains(out, "$42") {
		t2.Errorf("matches not surfaced: %q", out)
	}
	fb.findRes = nil
	out, isErr, _ := d.Dispatch(context.Background(), call("web_find", map[string]any{"query": "zzz"}))
	if isErr || !strings.Contains(out, "no matches") {
		t2.Errorf("want graceful no-match, got isErr=%v %q", isErr, out)
	}
}

func TestFind_RequiresQuery(t2 *testing.T) {
	d := disp(&fakeBrowser{}, Policy{})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_find", map[string]any{"query": "  "}))
	if !isErr || !strings.Contains(out, "query is required") {
		t2.Fatalf("got isErr=%v %q", isErr, out)
	}
}

func TestLinks(t2 *testing.T) {
	fb := &fakeBrowser{links: []Link{{Text: "Docs", Href: "https://x/docs"}, {Text: "", Href: "https://x/"}}}
	d := disp(fb, Policy{})
	out, _, _ := d.Dispatch(context.Background(), call("web_links", nil))
	if !strings.Contains(out, "2 link") || !strings.Contains(out, "Docs -> https://x/docs") || !strings.Contains(out, "(no text)") {
		t2.Errorf("links not formatted: %q", out)
	}
}

func TestUnknownTool(t2 *testing.T) {
	d := disp(&fakeBrowser{}, Policy{})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_teleport", nil))
	if !isErr || !strings.Contains(out, "unknown web tool") {
		t2.Fatalf("got isErr=%v %q", isErr, out)
	}
}

// --- the safety gate ---

func TestPolicyGate_ReadAlwaysAllowed(t2 *testing.T) {
	// Even with the most restrictive policy, read-class proceeds.
	d := disp(&fakeBrowser{navTitle: "ok"}, Policy{AllowInteract: false, Confirm: nil})
	_, isErr, _ := d.Dispatch(context.Background(), call("web_navigate", map[string]any{"url": "https://example.com"}))
	if isErr {
		t2.Fatal("read-class navigate must be allowed under any policy")
	}
}

func TestPolicyGate_TransactDeniedByDefault(t2 *testing.T) {
	// Confirm=nil ⇒ every transact action is denied. (No transact
	// tools ship this turn; assert the gate logic directly so the
	// safety contract is locked before those tools exist.)
	p := Policy{} // zero value = safest
	err := p.gate(context.Background(), ClassTransact, "web_purchase", "buy 1x widget $42")
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t2.Fatalf("transact must be denied by default, got %v", err)
	}
}

func TestPolicyGate_TransactAllowedOnlyWithConfirm(t2 *testing.T) {
	var sawAction, sawDetail string
	p := Policy{Confirm: func(_ context.Context, a, d string) bool {
		sawAction, sawDetail = a, d
		return true
	}}
	if err := p.gate(context.Background(), ClassTransact, "web_purchase", "buy widget"); err != nil {
		t2.Fatalf("confirmed transact should pass: %v", err)
	}
	// Action is the safety-category id (so the approval broker can map it);
	// the originating tool + detail ride in the detail string.
	if sawAction != "web_transact" || sawDetail != "web_purchase: buy widget" {
		t2.Errorf("Confirm got (%q,%q)", sawAction, sawDetail)
	}
	// Confirm returns false ⇒ blocked.
	p.Confirm = func(context.Context, string, string) bool { return false }
	if err := p.gate(context.Background(), ClassTransact, "web_purchase", "x"); err == nil {
		t2.Fatal("declined transact must be blocked")
	}
}

func TestPolicyGate_InteractRespectsFlag(t2 *testing.T) {
	deny := Policy{AllowInteract: false}
	if err := deny.gate(context.Background(), ClassInteract, "web_click", "btn"); err == nil {
		t2.Fatal("interact must be blocked when AllowInteract=false")
	}
	allow := Policy{AllowInteract: true}
	if err := allow.gate(context.Background(), ClassInteract, "web_click", "btn"); err != nil {
		t2.Fatalf("interact must pass when AllowInteract=true: %v", err)
	}
}

// --- tool specs ---

func TestTools_ReadLayerExposed(t2 *testing.T) {
	d := disp(&fakeBrowser{}, Policy{})
	names := map[string]bool{}
	for _, sp := range d.Tools() {
		names[sp.Name] = true
		if !json.Valid(sp.Parameters) {
			t2.Errorf("tool %s has invalid JSON-schema params", sp.Name)
		}
	}
	for _, want := range []string{"web_navigate", "web_read", "web_find", "web_links", "web_screenshot"} {
		if !names[want] {
			t2.Errorf("missing tool %s", want)
		}
	}
	// Transact tools must NOT be exposed this turn.
	for _, gone := range []string{"web_purchase", "web_submit"} {
		if names[gone] {
			t2.Errorf("transact tool %s exposed before its gated impl exists", gone)
		}
	}
}

func TestValidateURL(t2 *testing.T) {
	if _, err := validateURL("https://example.com/path?q=1"); err != nil {
		t2.Errorf("valid https rejected: %v", err)
	}
	for _, bad := range []string{"", "  ", "ftp://x", "javascript:1", "file:///x", "http://"} {
		if _, err := validateURL(bad); err == nil {
			t2.Errorf("%q should be invalid", bad)
		}
	}
}

// --- interact layer ---

func TestClick_NormalNeedsAllowInteract(t2 *testing.T) {
	fb := &fakeBrowser{locRes: LocateResult{Found: true, Tag: "a", Text: "More information"}, clickURL: "https://iana.org/"}
	// AllowInteract=false → benign click blocked.
	d := disp(fb, Policy{AllowInteract: false})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_click", map[string]any{"locator": "More information"}))
	if !isErr || !strings.Contains(out, "disabled by policy") {
		t2.Fatalf("benign click must be blocked when interact off: isErr=%v %q", isErr, out)
	}
	if fb.clicked != "" {
		t2.Fatal("Click() must NOT run when gate blocks")
	}
	// AllowInteract=true → proceeds.
	d = disp(fb, Policy{AllowInteract: true})
	out, isErr, _ = d.Dispatch(context.Background(), call("web_click", map[string]any{"locator": "More information"}))
	if isErr || !strings.Contains(out, "iana.org") {
		t2.Fatalf("allowed click failed: isErr=%v %q", isErr, out)
	}
	if fb.clicked != "More information" {
		t2.Errorf("Click() got %q", fb.clicked)
	}
}

func TestClick_DangerousEscalatesToTransact(t2 *testing.T) {
	// "Buy Now" must be transact-classified and DENIED even with
	// AllowInteract=true and even though it came via web_click.
	fb := &fakeBrowser{locRes: LocateResult{Found: true, Tag: "button", Text: "Buy Now"}, clickURL: "https://shop/done"}
	d := disp(fb, Policy{AllowInteract: true, Confirm: nil}) // confirm nil = deny transact
	out, isErr, _ := d.Dispatch(context.Background(), call("web_click", map[string]any{"locator": "Buy Now"}))
	if !isErr {
		t2.Fatalf("dangerous click must be blocked, got %q", out)
	}
	if !strings.Contains(out, "irreversible") && !strings.Contains(out, "not confirmed") {
		t2.Errorf("block reason should cite the safety boundary: %q", out)
	}
	if fb.clicked != "" {
		t2.Fatal("dangerous Click() must NOT execute")
	}
}

func TestClick_DangerousAllowedWithConfirm(t2 *testing.T) {
	var gotDetail string
	fb := &fakeBrowser{locRes: LocateResult{Found: true, Tag: "button", Text: "Checkout"}, clickURL: "https://shop/paid"}
	d := disp(fb, Policy{AllowInteract: true, Confirm: func(_ context.Context, a, det string) bool {
		gotDetail = det
		return true // approve
	}})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_click", map[string]any{"locator": "Checkout"}))
	if isErr {
		t2.Fatalf("confirmed transact click should proceed: %q", out)
	}
	if fb.clicked != "Checkout" {
		t2.Error("Click() should run after confirm")
	}
	if !strings.Contains(gotDetail, "IRREVERSIBLE") || !strings.Contains(gotDetail, "checkout") {
		t2.Errorf("Confirm detail should describe the danger: %q", gotDetail)
	}
}

func TestClick_NoMatch(t2 *testing.T) {
	d := disp(&fakeBrowser{locRes: LocateResult{Found: false}}, Policy{AllowInteract: true})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_click", map[string]any{"locator": "Nonexistent"}))
	if !isErr || !strings.Contains(out, "no clickable element") {
		t2.Fatalf("got isErr=%v %q", isErr, out)
	}
}

func TestFill_RedactsValue(t2 *testing.T) {
	fb := &fakeBrowser{}
	d := disp(fb, Policy{AllowInteract: true})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_fill", map[string]any{
		"locator": "Password", "value": "hunter2-secret",
	}))
	if isErr {
		t2.Fatalf("fill failed: %q", out)
	}
	if fb.filledVal != "hunter2-secret" {
		t2.Errorf("value not passed to browser: %q", fb.filledVal)
	}
	if strings.Contains(out, "hunter2-secret") {
		t2.Fatalf("SECRET LEAKED into tool result: %q", out)
	}
	if !strings.Contains(out, "14 chars") {
		t2.Errorf("expected char-count confirmation, got %q", out)
	}
}

func TestFill_BlockedWhenInteractOff(t2 *testing.T) {
	fb := &fakeBrowser{}
	d := disp(fb, Policy{AllowInteract: false})
	_, isErr, _ := d.Dispatch(context.Background(), call("web_fill", map[string]any{"locator": "q", "value": "x"}))
	if !isErr || fb.filledLoc != "" {
		t2.Fatalf("fill must be gated off: isErr=%v filled=%q", isErr, fb.filledLoc)
	}
}

func TestSelect(t2 *testing.T) {
	fb := &fakeBrowser{}
	d := disp(fb, Policy{AllowInteract: true})
	out, isErr, _ := d.Dispatch(context.Background(), call("web_select", map[string]any{
		"locator": "country", "option": "Canada",
	}))
	if isErr {
		t2.Fatalf("select failed: %q", out)
	}
	if fb.selLoc != "country" || fb.selOpt != "Canada" {
		t2.Errorf("select args wrong: %q %q", fb.selLoc, fb.selOpt)
	}
}

func TestClassifyClick(t2 *testing.T) {
	transact := []string{"Buy Now", "Proceed to Checkout", "Pay $42", "Place Order",
		"Delete account", "Sign in", "Log In", "Subscribe", "Confirm Order"}
	for _, s := range transact {
		if c, _ := classifyClick(s); c != ClassTransact {
			t2.Errorf("%q should be ClassTransact", s)
		}
	}
	interact := []string{"More information", "Next", "Show details", "Search", "Filter", "Read more"}
	for _, s := range interact {
		if c, _ := classifyClick(s); c != ClassInteract {
			t2.Errorf("%q should be ClassInteract", s)
		}
	}
}

func TestTools_InteractLayerExposed(t2 *testing.T) {
	d := disp(&fakeBrowser{}, Policy{})
	names := map[string]bool{}
	for _, sp := range d.Tools() {
		names[sp.Name] = true
	}
	for _, w := range []string{"web_click", "web_fill", "web_select"} {
		if !names[w] {
			t2.Errorf("missing interact tool %s", w)
		}
	}
	// transact tools still not exposed
	if names["web_purchase"] || names["web_submit"] {
		t2.Error("transact tools must not be exposed yet")
	}
}
