package main

import (
	"context"
	"strings"
	"testing"
)

// fakeTSCLI scripts the tailscale CLI: it records every invocation and returns
// canned combined-output / error keyed by the space-joined args.
type fakeTSCLI struct {
	calls [][]string
	resp  map[string]string
	errs  map[string]error
}

func (f *fakeTSCLI) run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	key := strings.Join(args, " ")
	return f.resp[key], f.errs[key]
}

func (f *fakeTSCLI) ran(want string) bool {
	for _, c := range f.calls {
		if strings.Join(c, " ") == want {
			return true
		}
	}
	return false
}

const tsRunningJSON = `{"BackendState":"Running","Self":{"DNSName":"host.tail47e9b.ts.net.","Online":true}}`

func newTSMgr(f *fakeTSCLI, bin string) *tailscaleManager {
	return &tailscaleManager{
		base:     context.Background(),
		bin:      bin,
		dashInfo: func() (bool, string) { return true, "127.0.0.1:8770" },
		run:      f.run,
	}
}

func TestTailscale_StatusConnectedNotServing(t *testing.T) {
	f := &fakeTSCLI{resp: map[string]string{
		"status --json": tsRunningJSON,
		"serve status":  "No serve config",
	}}
	st, _ := newTSMgr(f, "tailscale").Status()
	if !st.Installed || !st.LoggedIn {
		t.Fatalf("want installed+loggedIn, got %+v", st)
	}
	if st.DNSName != "host.tail47e9b.ts.net" { // trailing dot trimmed
		t.Errorf("DNSName not trimmed: %q", st.DNSName)
	}
	if st.URL != "https://host.tail47e9b.ts.net/" {
		t.Errorf("URL wrong: %q", st.URL)
	}
	if st.Serving {
		t.Error("should not be serving (No serve config)")
	}
}

func TestTailscale_StatusServing(t *testing.T) {
	f := &fakeTSCLI{resp: map[string]string{
		"status --json": tsRunningJSON,
		"serve status":  "https://host.tail47e9b.ts.net/ proxies to http://127.0.0.1:8770",
	}}
	st, _ := newTSMgr(f, "tailscale").Status()
	if !st.Serving {
		t.Error("serve status with an https mapping should report Serving")
	}
}

func TestTailscale_StatusNotInstalled(t *testing.T) {
	st, _ := newTSMgr(&fakeTSCLI{}, "").Status() // empty bin = not found
	if st.Installed || st.LoggedIn {
		t.Fatalf("empty bin must report not-installed: %+v", st)
	}
}

func TestTailscale_ServePublishesDashboardPort(t *testing.T) {
	f := &fakeTSCLI{resp: map[string]string{
		"status --json":   tsRunningJSON,
		"serve status":    "No serve config",
		"serve --bg 8770": "",
	}}
	url, err := newTSMgr(f, "tailscale").Serve()
	if err != nil {
		t.Fatalf("serve failed: %v", err)
	}
	if url != "https://host.tail47e9b.ts.net/" {
		t.Errorf("serve url wrong: %q", url)
	}
	if !f.ran("serve --bg 8770") {
		t.Errorf("must run `serve --bg 8770` (the dashboard port); calls=%v", f.calls)
	}
}

func TestTailscale_ServeFailsClosedWhenNotConnected(t *testing.T) {
	f := &fakeTSCLI{resp: map[string]string{
		"status --json": `{"BackendState":"Stopped","Self":{}}`,
	}}
	_, err := newTSMgr(f, "tailscale").Serve()
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("serve while Stopped must fail closed with guidance, got %v", err)
	}
	if f.ran("serve --bg 8770") {
		t.Error("must NOT publish when the backend is disconnected")
	}
}

func TestTailscale_ServeFailsClosedWhenMissing(t *testing.T) {
	_, err := newTSMgr(&fakeTSCLI{}, "").Serve()
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("serve with no CLI must error, got %v", err)
	}
}

func TestTailscale_Unserve(t *testing.T) {
	f := &fakeTSCLI{resp: map[string]string{"serve reset": ""}}
	if err := newTSMgr(f, "tailscale").Unserve(); err != nil {
		t.Fatalf("unserve failed: %v", err)
	}
	if !f.ran("serve reset") {
		t.Errorf("unserve must run `serve reset`; calls=%v", f.calls)
	}
}

func TestPortOf(t *testing.T) {
	if got := portOf("127.0.0.1:8770"); got != "8770" {
		t.Errorf("portOf loopback: %q", got)
	}
	if got := portOf("nonsense"); got != "" {
		t.Errorf("portOf bad addr should be empty: %q", got)
	}
}
