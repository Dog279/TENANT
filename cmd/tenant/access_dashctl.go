package main

// access_dashctl.go adapts the live iMessage allowlist/responder manager and the
// Discord relay manager to dashboard.AccessControl (TEN-208) — the web "Access"
// page. It is THIN: it only translates types (tui.PermissionInfo →
// dashboard.PermissionRow) and forwards to the existing managers' methods, which
// already own all persistence + locking + fail-soft behavior. This is the only
// place tui types touch the dashboard contract, so internal/dashboard never
// imports internal/tui.

import (
	"fmt"
	"runtime"

	"tenant/internal/dashboard"
	"tenant/internal/tui"
)

// dashAccess wraps the live managers behind dashboard.AccessControl. Both
// managers are always constructed in cmdTUI (cross-platform), so the forwards
// are safe; per-channel availability is surfaced via the *Available flags in
// View() and the managers themselves return friendly errors for platform/config
// -gated actions (responder off-macOS, relay without a token).
type dashAccess struct {
	im    *imessageAllowManager
	relay *discordRelayManager
}

var _ dashboard.AccessControl = dashAccess{}

// toPermRows translates the tui permission view into the dashboard-local one.
func toPermRows(p tui.PermissionControl) []dashboard.PermissionRow {
	if p == nil {
		return nil
	}
	in := p.Permissions()
	out := make([]dashboard.PermissionRow, 0, len(in))
	for _, pi := range in {
		out = append(out, dashboard.PermissionRow{Category: pi.Category, Mode: pi.Mode, Desc: pi.Desc})
	}
	return out
}

func (a dashAccess) View() dashboard.AccessView {
	v := dashboard.AccessView{}
	if a.im != nil {
		v.IMessageAvailable = a.im.Perms() != nil
		// The native responder is macOS-only; the allowlist + permissions are
		// cross-platform policy. Gate the responder toggle on the platform.
		v.ResponderAvailable = runtime.GOOS == "darwin"
		v.ResponderOn = a.im.ResponderOn()
		v.Handles = a.im.AllowList()
		v.IMessagePerms = toPermRows(a.im.Perms())
	}
	if a.relay != nil {
		v.DiscordAvailable = a.relay.Perms() != nil
		v.RelayConfigured = a.relay.Configured()
		running, opSet, execOn := a.relay.Status()
		v.RelayRunning = running
		v.OperatorSet = opSet
		v.ExecOn = execOn
		v.OperatorID = a.relay.OperatorID()
		v.DiscordPerms = toPermRows(a.relay.Perms())
	}
	return v
}

// The mutation methods nil-guard their manager before forwarding. In production
// both managers are always constructed in cmdTUI, but the guard keeps the
// adapter safe under partial wiring / tests (and mirrors View()'s own guards).

func (a dashAccess) IMessageAllow(h string) (string, bool, error) {
	if a.im == nil {
		return "", false, errIMessageUnavailable
	}
	return a.im.Allow(h)
}
func (a dashAccess) IMessageDeny(h string) (string, bool, error) {
	if a.im == nil {
		return "", false, errIMessageUnavailable
	}
	return a.im.Deny(h)
}
func (a dashAccess) IMessageClear() (int, error) {
	if a.im == nil {
		return 0, errIMessageUnavailable
	}
	return a.im.Clear()
}
func (a dashAccess) SetIMessageResponder(on bool) (string, error) {
	if a.im == nil {
		return "", errIMessageUnavailable
	}
	return a.im.SetResponder(on)
}

func (a dashAccess) SetIMessagePermission(cat, mode string) (bool, error) {
	if a.im == nil {
		return false, errIMessageUnavailable
	}
	p := a.im.Perms()
	if p == nil {
		return false, errIMessageUnavailable
	}
	return p.SetPermission(cat, mode)
}

func (a dashAccess) SetRelayEnabled(on bool) error {
	if a.relay == nil {
		return errDiscordUnavailable
	}
	if on {
		return a.relay.Enable()
	}
	return a.relay.Disable()
}
func (a dashAccess) SetRelayOperator(id string) error {
	if a.relay == nil {
		return errDiscordUnavailable
	}
	return a.relay.SetOperator(id)
}
func (a dashAccess) SetRelayExec(on bool) error {
	if a.relay == nil {
		return errDiscordUnavailable
	}
	return a.relay.SetExec(on)
}

func (a dashAccess) SetRelayPermission(cat, mode string) (bool, error) {
	if a.relay == nil {
		return false, errDiscordUnavailable
	}
	p := a.relay.Perms()
	if p == nil {
		return false, errDiscordUnavailable
	}
	return p.SetPermission(cat, mode)
}

var (
	errIMessageUnavailable = fmt.Errorf("imessage permissions unavailable here (native transport is macOS-only)")
	errDiscordUnavailable  = fmt.Errorf("discord not configured")
)
