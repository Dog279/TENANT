package dashboard

// access.go is the server-rendered "Access" admin section (TEN-208): manage who
// can reach the agent over the offsite channels, from the web GUI. It mirrors
// the TUI's /imessage and /relay commands — the iMessage drive-allowlist +
// responder + per-category permissions, and the Discord relay (operator /
// on-off / exec / per-category permissions). Same idiom as cron.go: an
// AccessControl interface (satisfied in cmd/tenant via dashAccess), unconditional
// route mounting with nil-guarded handlers, and form mutations answered by
// formDone: in place when Datastar posts them (the page keeps its scroll and a
// toast says what happened), else a 303 so a refresh can't double-submit. All
// values render through html/template auto-escaping (no template.HTML) — a
// hostile handle/operator-id can't inject markup. Every route rides the
// dashboard's existing auth (Run wraps the mux in secure()); mutations are POST.

import (
	"fmt"
	"net/http"
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
	SetIMessagePassive(on bool) (msg string, err error) // TEN-485: read only vs answer
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
	Title    string // plain name: "Run commands"
	Can      string // what Allow lets the agent do
	New      bool   // an update added it and nobody chose it, so it asks (TEN-343)
}

// AccessView is the whole page in one struct. The *Available / *Configured
// flags drive per-channel graceful degradation; when false the section renders
// a muted note and its action forms are omitted.
type AccessView struct {
	// iMessage
	IMessageAvailable  bool // the allowlist + permissions surface is wired
	ResponderAvailable bool // the native responder is reachable here (macOS only)
	ResponderOn        bool
	Passive            bool     // the responder reads every text and answers none (TEN-485)
	Handles            []string // sorted, normalized drive-allowlist
	SingleHandle       bool     // exactly one handle ⇒ deny confirms ("removing the last entry")
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
	// Letting someone in, cutting everyone off, and loosening what a channel
	// may do all confirm (TEN-211); turning things off and asking or
	// blocking don't. Removing one person confirms only when they're the
	// last, which cuts everyone off.
	on := confirmUnless("on", "", "false", "off")
	loosens := confirmUnless("mode", "ask", "deny")
	lastHandle := func(*http.Request) bool { return s.access != nil && s.access.View().SingleHandle }
	// iMessage
	mux.HandleFunc("POST /access/imessage/allow", needConfirm(nil, s.handleIMessageAllowForm))
	mux.HandleFunc("POST /access/imessage/deny", needConfirm(lastHandle, s.handleIMessageDenyForm))
	mux.HandleFunc("POST /access/imessage/clear", needConfirm(nil, s.handleIMessageClearForm))
	mux.HandleFunc("POST /access/imessage/responder", needConfirm(on, s.handleIMessageResponderForm))
	mux.HandleFunc("POST /access/imessage/passive", needConfirm(confirmUnless("on", "true", "on"), s.handleIMessagePassiveForm))
	mux.HandleFunc("POST /access/imessage/permission", needConfirm(loosens, s.handleIMessagePermissionForm))
	// Discord
	mux.HandleFunc("POST /access/discord/enable", needConfirm(on, s.handleRelayEnableForm))
	mux.HandleFunc("POST /access/discord/operator", needConfirm(nil, s.handleRelayOperatorForm))
	mux.HandleFunc("POST /access/discord/exec", needConfirm(on, s.handleRelayExecForm))
	mux.HandleFunc("POST /access/discord/permission", needConfirm(loosens, s.handleRelayPermissionForm))
}

type accessPageData struct {
	layoutData
	Configured bool
	View       AccessView
	Err        string
	Msg        string
}

func (s *Server) accessData() accessPageData {
	d := accessPageData{layoutData: layoutData{Title: "Access", Page: "access"}}
	if s.access != nil {
		d.Configured = true
		d.View = s.access.View()
	}
	return d
}

func (s *Server) handleAccessPage(w http.ResponseWriter, r *http.Request) {
	d := s.accessData()
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	s.render(w, s.tmpl.access, d)
}

// accessDone is every Access form's answer: in place for a Datastar post,
// with #access-body re-rendered; a 303 with the flash otherwise.
func (s *Server) accessDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/access", msg, errMsg, func() (string, error) {
		return part(s.tmpl.access, "access-body", s.accessData())
	})
}

// --- iMessage POST handlers ---

func (s *Server) handleIMessageAllowForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	handle := strings.TrimSpace(r.FormValue("handle"))
	if handle == "" {
		s.accessDone(w, r, "", "Enter a phone number or email to allow.")
		return
	}
	norm, added, err := s.access.IMessageAllow(handle)
	if err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "imessage allow", err, "Couldn't save that. The Logs page has the reason."))
		return
	}
	if !added {
		s.accessDone(w, r, norm+" can already message your agent.", "")
		return
	}
	s.accessDone(w, r, norm+" can now message your agent on iMessage.", "")
}

func (s *Server) handleIMessageDenyForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	handle := strings.TrimSpace(r.FormValue("handle"))
	if handle == "" {
		s.accessDone(w, r, "", "Enter a phone number or email to remove.")
		return
	}
	norm, removed, err := s.access.IMessageDeny(handle)
	if err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "imessage deny", err, "Couldn't remove "+handle+"."))
		return
	}
	if !removed {
		s.accessDone(w, r, norm+" wasn't on the list.", "")
		return
	}
	s.accessDone(w, r, norm+" can no longer message your agent.", "")
}

func (s *Server) handleIMessageClearForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	n, err := s.access.IMessageClear()
	if err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "imessage clear", err, "Couldn't clear the list."))
		return
	}
	s.accessDone(w, r, fmt.Sprintf("Removed everyone (%d). Nobody can message your agent on iMessage until you add someone.", n), "")
}

func (s *Server) handleIMessageResponderForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	on := formOn(r)
	if _, err := s.access.SetIMessageResponder(on); err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "imessage responder", err, "Couldn't change whether your agent answers iMessages."))
		return
	}
	if on {
		s.accessDone(w, r, "Your agent answers iMessages from the people listed below.", "")
		return
	}
	s.accessDone(w, r, "Your agent no longer answers iMessages.", "")
}

// handleIMessagePassiveForm switches between reading only (on=true) and
// answering (on=false; that loosens, so it confirms). Either starts the
// responder if it is off.
func (s *Server) handleIMessagePassiveForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	on := formOn(r)
	if _, err := s.access.SetIMessagePassive(on); err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "imessage mode", err, "Couldn't change how your agent handles iMessages."))
		return
	}
	if on {
		s.accessDone(w, r, "Your agent reads your iMessages and answers none.", "")
		return
	}
	s.accessDone(w, r, "Your agent answers iMessages from the people listed below. Everyone else's are only read.", "")
}

func (s *Server) handleIMessagePermissionForm(w http.ResponseWriter, r *http.Request) {
	s.permissionForm(w, r, "iMessage", "imessage permission", func(v AccessView) []PermissionRow { return v.IMessagePerms }, AccessControl.SetIMessagePermission)
}

// --- Discord relay POST handlers ---

func (s *Server) handleRelayEnableForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	on := formOn(r)
	if err := s.access.SetRelayEnabled(on); err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "relay enable", err, "Couldn't change the Discord relay. It needs your Discord user ID, set below."))
		return
	}
	if on {
		s.accessDone(w, r, "The Discord relay is starting.", "")
	} else {
		s.accessDone(w, r, "The Discord relay stopped.", "")
	}
}

func (s *Server) handleRelayOperatorForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	id := strings.TrimSpace(r.FormValue("operator"))
	if id == "" {
		s.accessDone(w, r, "", "Enter your Discord user ID.")
		return
	}
	if err := s.access.SetRelayOperator(id); err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "relay operator", err, "Couldn't save that Discord user ID."))
		return
	}
	s.accessDone(w, r, "Saved. Only that Discord account can approve your agent's risky actions from Discord.", "")
}

func (s *Server) handleRelayExecForm(w http.ResponseWriter, r *http.Request) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	on := formOn(r)
	if err := s.access.SetRelayExec(on); err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), "relay exec", err, "Couldn't change whether Discord messages may make changes."))
		return
	}
	if on {
		s.accessDone(w, r, "Discord messages may make changes now, each as its safety setting below says.", "")
	} else {
		s.accessDone(w, r, "Discord messages can't make changes now: running commands, changing files, irreversible actions, leaving the sandbox and sending messages are refused.", "")
	}
}

func (s *Server) handleRelayPermissionForm(w http.ResponseWriter, r *http.Request) {
	s.permissionForm(w, r, "Discord", "relay permission", func(v AccessView) []PermissionRow { return v.DiscordPerms }, AccessControl.SetRelayPermission)
}

// modeWords says what a safety setting does, for the flash after a change.
var modeWords = map[string]string{"ask": "asks you first", "allow": "is allowed without asking", "deny": "is blocked"}

// permissionForm sets one channel's safety setting for one category. The
// mode must be exactly ask, allow or deny after trimming and lowercasing;
// anything else is refused before the control sees it (D2).
func (s *Server) permissionForm(w http.ResponseWriter, r *http.Request, channel, action string,
	rows func(AccessView) []PermissionRow, set func(c AccessControl, category, mode string) (bool, error)) {
	if s.access == nil {
		s.accessDone(w, r, "", "")
		return
	}
	cat := strings.TrimSpace(r.FormValue("category"))
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if modeWords[mode] == "" {
		s.accessDone(w, r, "", "Pick Ask first, Allow or Block.")
		return
	}
	if _, err := set(s.access, cat, mode); err != nil {
		s.accessDone(w, r, "", s.flashFor(r.Context(), action, err, "Couldn't change that safety setting."))
		return
	}
	title := cat
	for _, p := range rows(s.access.View()) {
		if p.Category == cat && p.Title != "" {
			title = p.Title
		}
	}
	s.accessDone(w, r, channel+" conversations: "+title+" "+modeWords[mode]+".", "")
}

// formOn parses a boolean toggle field (mirrors the cron exec checkbox parse).
func formOn(r *http.Request) bool {
	v := r.FormValue("on")
	return v == "true" || v == "on"
}
