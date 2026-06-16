package dashboard

// access.go is the server-rendered "Access" admin section (TEN-208): manage who
// can reach the agent over the offsite channels, from the web GUI. It mirrors
// the TUI's /imessage and /relay commands — the iMessage drive-allowlist +
// responder + per-category permissions, and the Discord relay (operator /
// on-off / exec / per-category permissions). Same idiom as cron.go: an
// AccessControl interface (satisfied in cmd/tenant via dashAccess), unconditional
// route mounting with nil-guarded handlers, and form/303 mutations so every view
// is a plain GET httptest can assert on and a refresh can't double-submit. All
// values render through html/template auto-escaping (no template.HTML) — a
// hostile handle/operator-id can't inject markup. Every route rides the
// dashboard's existing auth (Run wraps the mux in secure()); mutations are POST.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// AccessControl is the runtime Access surface the dashboard drives. cmd/tenant
// adapts its imessageAllowManager + discordRelayManager to it (see dashAccess).
// A nil control renders an "isn't configured" state rather than 404ing the nav
// link; each channel degrades independently via the *Available flags on
// AccessView (iMessage responder is macOS-only; Discord needs a bot token).
type AccessControl interface {
	// View is the whole render-ready page state in one read (called per GET).
	View() AccessView

	// iMessage drive-allowlist (deny-by-default).
	IMessageAllow(handle string) (norm string, added bool, err error)
	IMessageDeny(handle string) (norm string, removed bool, err error)
	IMessageClear() (removed int, err error)
	// iMessage autonomous responder + per-category permissions.
	SetIMessageResponder(on bool) (msg string, err error)
	SetIMessagePermission(category, mode string) (changed bool, err error)

	// Discord relay.
	SetRelayEnabled(on bool) error
	SetRelayOperator(id string) error
	SetRelayExec(on bool) error
	SetRelayPermission(category, mode string) (changed bool, err error)
}

// PermissionRow is one safety category's current mode, render-ready. It mirrors
// tui.PermissionInfo but lives in this package so the dashboard never imports
// internal/tui; the cmd/tenant adapter translates between them.
type PermissionRow struct {
	Category string
	Mode     string // "ask" | "allow" | "deny"
	Desc     string
}

// AccessView is the whole page in one struct. The *Available / *Configured
// flags drive per-channel graceful degradation; when false the section renders
// a muted note and its action forms are omitted.
type AccessView struct {
	// iMessage
	IMessageAvailable  bool // the allowlist + permissions surface is wired
	ResponderAvailable bool // the native responder is reachable here (macOS only)
	ResponderOn        bool
	Handles            []string // sorted, normalized drive-allowlist
	IMessagePerms      []PermissionRow

	// Discord
	DiscordAvailable bool // the relay permission surface is wired
	RelayConfigured  bool // a bot token is set (the relay can actually run)
	RelayRunning     bool
	OperatorSet      bool
	OperatorID       string
	ExecOn           bool
	DiscordPerms     []PermissionRow
}

// SetAccess installs the access control after construction (mirrors SetCron).
// Safe to call before serving; handlers nil-guard until it's set.
func (s *Server) SetAccess(c AccessControl) { s.access = c }

// mountAccessSSR registers the Access page + its form actions. Mounted
// unconditionally; handlers nil-guard on s.access.
func (s *Server) mountAccessSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /access", s.handleAccessPage)
	// iMessage
	mux.HandleFunc("POST /access/imessage/allow", s.handleIMessageAllowForm)
	mux.HandleFunc("POST /access/imessage/deny", s.handleIMessageDenyForm)
	mux.HandleFunc("POST /access/imessage/clear", s.handleIMessageClearForm)
	mux.HandleFunc("POST /access/imessage/responder", s.handleIMessageResponderForm)
	mux.HandleFunc("POST /access/imessage/permission", s.handleIMessagePermissionForm)
	// Discord
	mux.HandleFunc("POST /access/discord/enable", s.handleRelayEnableForm)
	mux.HandleFunc("POST /access/discord/operator", s.handleRelayOperatorForm)
	mux.HandleFunc("POST /access/discord/exec", s.handleRelayExecForm)
	mux.HandleFunc("POST /access/discord/permission", s.handleRelayPermissionForm)
}

type accessPageData struct {
	layoutData
	Configured bool
	View       AccessView
	Err        string
	Msg        string
}

func (s *Server) handleAccessPage(w http.ResponseWriter, r *http.Request) {
	d := accessPageData{layoutData: layoutData{Title: "Access", Page: "access"}}
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	if s.access != nil {
		d.Configured = true
		d.View = s.access.View()
	}
	s.render(w, s.tmpl.access, d)
}

// --- iMessage POST handlers (form/303) ---

func (s *Server) handleIMessageAllowForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	handle := strings.TrimSpace(r.FormValue("handle"))
	if handle == "" {
		redirectAccess(w, r, "", "Enter a phone number or email to allow.")
		return
	}
	norm, added, err := s.access.IMessageAllow(handle)
	if err != nil {
		s.log.Warn("dashboard: imessage allow", "err", err)
		redirectAccess(w, r, "", "Couldn't allow that handle: "+err.Error())
		return
	}
	if !added {
		redirectAccess(w, r, norm+" is already allowed.", "")
		return
	}
	redirectAccess(w, r, "Allowed "+norm+".", "")
}

func (s *Server) handleIMessageDenyForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	handle := strings.TrimSpace(r.FormValue("handle"))
	if handle == "" {
		redirectAccess(w, r, "", "Enter a phone number or email to deny.")
		return
	}
	norm, removed, err := s.access.IMessageDeny(handle)
	if err != nil {
		s.log.Warn("dashboard: imessage deny", "err", err)
		redirectAccess(w, r, "", "Couldn't remove that handle: "+err.Error())
		return
	}
	if !removed {
		redirectAccess(w, r, norm+" wasn't on the allowlist.", "")
		return
	}
	redirectAccess(w, r, "Removed "+norm+".", "")
}

func (s *Server) handleIMessageClearForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	n, err := s.access.IMessageClear()
	if err != nil {
		s.log.Warn("dashboard: imessage clear", "err", err)
		redirectAccess(w, r, "", "Couldn't clear the allowlist: "+err.Error())
		return
	}
	redirectAccess(w, r, fmt.Sprintf("Cleared %d handle(s).", n), "")
}

func (s *Server) handleIMessageResponderForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	on := formOn(r)
	msg, err := s.access.SetIMessageResponder(on)
	if err != nil {
		s.log.Warn("dashboard: imessage responder", "on", on, "err", err)
		redirectAccess(w, r, "", "Couldn't change the responder: "+err.Error())
		return
	}
	if msg == "" {
		if on {
			msg = "iMessage responder on."
		} else {
			msg = "iMessage responder off."
		}
	}
	redirectAccess(w, r, msg, "")
}

func (s *Server) handleIMessagePermissionForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	cat := strings.TrimSpace(r.FormValue("category"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	if _, err := s.access.SetIMessagePermission(cat, mode); err != nil {
		s.log.Warn("dashboard: imessage permission", "cat", cat, "err", err)
		redirectAccess(w, r, "", "Couldn't update that permission: "+err.Error())
		return
	}
	redirectAccess(w, r, "🔐 iMessage "+cat+" → "+mode, "")
}

// --- Discord relay POST handlers (form/303) ---

func (s *Server) handleRelayEnableForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	on := formOn(r)
	if err := s.access.SetRelayEnabled(on); err != nil {
		s.log.Warn("dashboard: relay enable", "on", on, "err", err)
		redirectAccess(w, r, "", "Couldn't change the relay: "+err.Error())
		return
	}
	if on {
		redirectAccess(w, r, "Discord relay starting.", "")
	} else {
		redirectAccess(w, r, "Discord relay stopped.", "")
	}
}

func (s *Server) handleRelayOperatorForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	if err := s.access.SetRelayOperator(strings.TrimSpace(r.FormValue("operator"))); err != nil {
		s.log.Warn("dashboard: relay operator", "err", err)
		redirectAccess(w, r, "", "Couldn't set the operator: "+err.Error())
		return
	}
	redirectAccess(w, r, "Operator set.", "")
}

func (s *Server) handleRelayExecForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	on := formOn(r)
	if err := s.access.SetRelayExec(on); err != nil {
		s.log.Warn("dashboard: relay exec", "on", on, "err", err)
		redirectAccess(w, r, "", "Couldn't change exec mode: "+err.Error())
		return
	}
	if on {
		redirectAccess(w, r, "Discord exec mode on (dangerous tools reachable, each per-action approved).", "")
	} else {
		redirectAccess(w, r, "Discord exec mode off.", "")
	}
}

func (s *Server) handleRelayPermissionForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		http.Redirect(w, r, "/access", http.StatusSeeOther)
		return
	}
	cat := strings.TrimSpace(r.FormValue("category"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	if _, err := s.access.SetRelayPermission(cat, mode); err != nil {
		s.log.Warn("dashboard: relay permission", "cat", cat, "err", err)
		redirectAccess(w, r, "", "Couldn't update that permission: "+err.Error())
		return
	}
	redirectAccess(w, r, "🔐 Discord "+cat+" → "+mode, "")
}

// formOn parses a boolean toggle field (mirrors the cron exec checkbox parse).
func formOn(r *http.Request) bool {
	v := r.FormValue("on")
	return v == "true" || v == "on"
}

// redirectAccess 303-redirects back to /access with an optional flash msg/err.
func redirectAccess(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/access"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
