package main

// tailscalemgr.go (TEN-233) is the operator-driven control behind `/tailscale`:
// publish the LOOPBACK dashboard onto the tailnet using `tailscale serve` — the
// Tailscale reverse proxy. This deliberately does NOT change the dashboard's
// bind (it stays 127.0.0.1, so the dashboard's non-loopback safety guard never
// trips and there's no cert/token to manage): Tailscale terminates HTTPS at the
// tailnet edge and the tailnet ACL is the access control. Reachable only from
// the operator's own tailnet devices (serve, not funnel — never the public net).
//
// Preconditions, surfaced as clear errors (never silently "succeed"):
//   - the tailscale CLI must be resolvable (PATH, the macOS app bundle, or
//     $TAILSCALE_CLI); and
//   - the backend must be connected (BackendState == "Running", i.e. `tailscale
//     up` has been run / the GUI app is logged in).
//
// Tailscale persists the serve config in tailscaled across reboots, so Tenant
// holds no state here — this is a thin, testable convenience wrapper over the CLI.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"tenant/internal/tui"
)

// macOSTailscaleBundle is where the macOS GUI app ships its CLI when it isn't
// symlinked into PATH (the common "Tailscale is installed but `tailscale` isn't
// found" case the operator hit).
const macOSTailscaleBundle = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// tailscaleManager implements tui.TailscaleControl over the local tailscale CLI.
type tailscaleManager struct {
	base context.Context
	bin  string // resolved tailscale binary ("" ⇒ not found)
	// dashInfo reports the dashboard's (running, addr) so serve can target its
	// loopback port and warn when the dashboard is stopped. Set to dashMgr.Status.
	dashInfo func() (running bool, addr string)
	// run is the exec seam (overridden in tests). It runs `bin args...` and
	// returns combined output. nil ⇒ execTailscale.
	run func(ctx context.Context, bin string, args ...string) (string, error)
	// persist records the serve on/off choice to config so it survives a relaunch
	// (TEN-233). Only called on a SUCCESSFUL serve/unserve, so a transient failure
	// (e.g. tailscale not up yet at launch) never flips the operator's intent.
	persist func(serve bool)
	log     *slog.Logger
}

func newTailscaleManager(base context.Context, dashInfo func() (bool, string), log *slog.Logger) *tailscaleManager {
	return &tailscaleManager{base: base, bin: resolveTailscaleBin(), dashInfo: dashInfo, run: execTailscale, log: log}
}

// resolveTailscaleBin finds the tailscale CLI: an explicit $TAILSCALE_CLI wins,
// then PATH, then the macOS app bundle. "" when none exists.
func resolveTailscaleBin() string {
	if v := strings.TrimSpace(os.Getenv("TAILSCALE_CLI")); v != "" {
		return v
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	if fi, err := os.Stat(macOSTailscaleBundle); err == nil && !fi.IsDir() {
		return macOSTailscaleBundle
	}
	return ""
}

// execTailscale runs the CLI with a bounded timeout, returning combined output.
func execTailscale(ctx context.Context, bin string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, args...).CombinedOutput()
	return string(out), err
}

// Status queries the CLI for install/connection/serve state. It never errors on
// a reachable-but-unhappy CLI — it reports the condition via the struct so the
// TUI can guide the operator. (The error return is reserved for future use.)
func (m *tailscaleManager) Status() (tui.TailscaleStatus, error) {
	if m.bin == "" {
		return tui.TailscaleStatus{Installed: false, Detail: "tailscale CLI not found"}, nil
	}
	st := tui.TailscaleStatus{Installed: true}

	out, err := m.run(m.base, m.bin, "status", "--json")
	if err != nil {
		st.Detail = "tailscale status failed: " + firstLine(out, err)
		return st, nil
	}
	var js struct {
		BackendState string
		Self         struct {
			DNSName string
			Online  bool
		}
	}
	if jerr := json.Unmarshal([]byte(out), &js); jerr != nil {
		st.Detail = "could not parse `tailscale status --json`"
		return st, nil
	}
	st.State = js.BackendState
	st.LoggedIn = js.BackendState == "Running"
	st.DNSName = strings.TrimSuffix(js.Self.DNSName, ".")
	if st.LoggedIn && st.DNSName != "" {
		st.URL = "https://" + st.DNSName + "/"
	}

	// serve active? `tailscale serve status` prints "No serve config" when off.
	if so, serr := m.run(m.base, m.bin, "serve", "status"); serr == nil {
		st.Serving = strings.Contains(so, "https://")
	}
	return st, nil
}

// Serve publishes the dashboard's loopback port onto the tailnet via
// `tailscale serve --bg <port>`. Fails closed on a missing CLI or a
// disconnected backend, with operator-actionable guidance.
func (m *tailscaleManager) Serve() (string, error) {
	st, _ := m.Status()
	if !st.Installed {
		return "", fmt.Errorf("tailscale CLI not found — install Tailscale, or on macOS symlink the GUI app's CLI (%s) into your PATH, then retry", macOSTailscaleBundle)
	}
	if !st.LoggedIn {
		return "", fmt.Errorf("tailscale is installed but not connected (state: %s) — run `tailscale up` or open the app and log in, then retry", orUnknown(st.State))
	}
	_, addr := m.dashInfo()
	port := portOf(addr)
	if port == "" {
		return "", fmt.Errorf("could not determine the dashboard port from %q", addr)
	}
	if out, err := m.run(m.base, m.bin, "serve", "--bg", port); err != nil {
		return "", fmt.Errorf("`tailscale serve --bg %s` failed: %s", port, firstLine(out, err))
	}
	if m.persist != nil {
		m.persist(true)
	}
	url := st.URL
	if url == "" {
		url = "https://" + st.DNSName + "/"
	}
	return url, nil
}

// Unserve tears down the tailnet serve config (`tailscale serve reset`).
func (m *tailscaleManager) Unserve() error {
	if m.bin == "" {
		return fmt.Errorf("tailscale CLI not found")
	}
	if out, err := m.run(m.base, m.bin, "serve", "reset"); err != nil {
		return fmt.Errorf("`tailscale serve reset` failed: %s", firstLine(out, err))
	}
	if m.persist != nil {
		m.persist(false)
	}
	return nil
}

// reassertOnLaunch re-applies a persisted serve choice at startup (best-effort).
// Idempotent — `tailscale serve` re-asserts the same config harmlessly when it's
// already active. A failure (e.g. tailscale not connected yet) is returned for a
// feed note and does NOT clear the persisted intent. Caller guards on the flag.
func (m *tailscaleManager) reassertOnLaunch() (string, error) {
	return m.Serve()
}

// --- small helpers ---

// portOf extracts the port from a host:port listen address.
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(strings.TrimSpace(addr)); err == nil {
		return port
	}
	return ""
}

// firstLine returns the first non-empty line of CLI output, falling back to the
// exec error — so a failure always carries SOME diagnostic.
func firstLine(out string, err error) string {
	for _, ln := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			return s
		}
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}
