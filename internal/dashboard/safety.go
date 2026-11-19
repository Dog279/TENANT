package dashboard

// safety.go is the "Approvals & safety" page (TEN-207, TEN-343): the live
// queue of requests waiting for the operator's OK and the per-category modes
// of the main broker. The queue is re-rendered whole from Pending(), the
// source of truth, on the page, on every approval event and every
// queueResync over /events (ssr_chat.go), and after a decision; its buttons
// are Datastar posts, so the same partial works above the Chat composer
// without a redirect wiping the transcript. The mode forms answer in place
// too (safetyDone), re-rendering only #safety-modes.

import (
	"bytes"
	"cmp"
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"tenant/internal/control"
)

// SafetyControl is the main approval broker's category modes. cmd/tenant
// passes the broker itself: it implements both methods.
type SafetyControl interface {
	Permissions() []control.PermissionInfo
	SetPermissionFrom(surface, category, mode string) (changed bool, err error)
}

// SafetyFacts are what the broker doesn't know but a consequence line must
// say (D11): who commands run as, and whether they run in the sandbox.
type SafetyFacts struct {
	Account   string // "NT AUTHORITY\\SYSTEM", "dylan"; "" = unknown
	System    bool   // Windows' SYSTEM account (by SID): full control of the computer
	Sandboxed bool   // the exec profile for dashboard-driven turns is restricted and enforced
}

// ApprovalItem is one queued request, render-ready.
type ApprovalItem struct {
	ID, Category, Title, Action, Detail string
	From                                string // "" = this computer, "iMessage"; none for pairing
	Machine                             string // the fleet machine the action lands on, by its id; "" = this computer
	Waited                              string // waited(AgeSecs)
	Can                                 string
	Lines                               int  // Detail line count; > 8 adds "This request is N lines long. Read all of it before you allow it." (over 400 bytes on fewer lines: "This request is long.")
	Standing                            bool // modes wired && SessionOK && Origin=="local" && no fleet machine && cat != sandbox && !(cat == exec && !sandboxed(…))
	Pairing                             bool
	Actionable                          bool // ID matches ^[A-Za-z0-9-]+$
}

// DecideURL is the post target of one decision button. The ID is escaped
// even though only Actionable IDs get buttons (F1), and a decision behind a
// confirm step carries confirm=yes: the button sits inside that step.
func (it ApprovalItem) DecideURL(decision string) string {
	q := url.Values{"decision": {decision}}
	if decisionNeedsConfirm(decision, it.Pairing) {
		q.Set("confirm", "yes")
	}
	return "/safety/approvals/" + url.PathEscape(it.ID) + "?" + q.Encode()
}

// ModeRow is one category's mode row, render-ready.
type ModeRow struct {
	Category, Title, Can, Mode, Consequence string
	New, Session                            bool
	Discord                                 string // relay mode for this category, "" when the relay isn't running
	Allow                                   string // the Allow confirm step's consequence, built from the facts
}

// SetSafety installs the broker and the facts about it (facts may be nil).
func (s *Server) SetSafety(c SafetyControl, facts func() SafetyFacts) {
	s.safety, s.safetyFacts = c, facts
}

func (s *Server) mountSafetySSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /safety", s.handleSafetyPage)
	mux.HandleFunc("POST /safety/mode", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleSafetyModeForm))
	mux.HandleFunc("POST /safety/new", s.handleSafetyKeepNewForm)
	mux.HandleFunc("POST /safety/approvals/{id}", needConfirm(func(r *http.Request) bool {
		return decisionNeedsConfirm(r.FormValue("decision"), s.isPairing(r.PathValue("id")))
	}, s.handleSafetyDecide))
}

// decisionNeedsConfirm is the queue's confirm rule (D4): Deny and a plain
// Allow once are one tap; pairing's Allow once, the session grant and any
// other value need the step. Fail-closed: an unknown value confirms, then
// the handler refuses it.
func decisionNeedsConfirm(decision string, pairing bool) bool {
	return decision != "deny" && (decision != "approve" || pairing)
}

func (s *Server) isPairing(id string) bool {
	if s.approvals == nil {
		return false
	}
	for _, p := range s.approvals.Pending() {
		if p.ID == id {
			return p.Category == "pairing"
		}
	}
	return false
}

// The queue's and the modes' fixed words (DESIGN.md §2).
const (
	approvalsOff    = "Approvals aren't available on this Tenant."
	modesOff        = "Safety settings aren't available on this Tenant."
	alreadyAnswered = "This was already answered (here, in another tab, or in the terminal) or it timed out. This tap didn't change anything."
	notAvailable    = "That choice isn't available for this request. Nothing was allowed."
)

// decisionDone is what each decision the page offers says once made. It is
// also the check: approve_always and anything else is refused before Decide
// (D4: "Always" has no honest consequence line on the web).
var decisionDone = map[string]string{
	"approve":         "Allowed once. Your agent is carrying on.",
	"approve_session": "Allowed for this session.",
	"deny":            "Denied. Your agent was told no.",
}

// safeID is what an approval ID may be to go in a Datastar expression.
var safeID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// approvalQueue is the approvalq partial's input.
type approvalQueue struct {
	Configured bool
	Items      []ApprovalItem
	Relay      bool // the Discord relay runs: say where its requests are answered (D6)
	Msg, Err   string
}

// queue reads the approval queue, oldest first then by ID (Pending iterates
// a map, so rows would reshuffle on every patch otherwise).
func (s *Server) queue() approvalQueue {
	q := approvalQueue{Configured: s.approvals != nil}
	if s.access != nil {
		q.Relay = s.access.View().RelayRunning
	}
	if s.approvals == nil {
		return q
	}
	can, boxed := map[string]string{}, false
	if s.safety != nil {
		perms := s.safety.Permissions()
		for _, p := range perms {
			can[p.Category] = p.Can
		}
		boxed = sandboxed(s.facts(), perms)
	}
	pending := slices.Clone(s.approvals.Pending())
	slices.SortFunc(pending, func(a, b PendingApproval) int {
		return cmp.Or(cmp.Compare(b.AgeSecs, a.AgeSecs), strings.Compare(a.ID, b.ID))
	})
	for _, p := range pending {
		it := ApprovalItem{
			ID: p.ID, Category: p.Category, Action: p.Action, Detail: p.Detail,
			Waited: waited(p.AgeSecs), Can: can[p.Category],
			Lines:      strings.Count(p.Detail, "\n") + 1,
			Pairing:    p.Category == "pairing",
			Actionable: safeID.MatchString(p.ID),
		}
		// A grant lives on the broker that asked: only the main one's shows
		// (and can be revoked) on the modes card. Unsandboxed commands get
		// no session step: with it, the agent could answer its own requests.
		// Nor does Leave the sandbox: its grant re-runs every command the box
		// refuses on the computer itself, unasked, which is the same thing.
		// Nor does an action on a fleet machine: the modes card is this
		// computer's, and a machine keeps no session grant.
		it.Standing = s.safety != nil && p.SessionOK && p.Origin == "local" && p.Host == "" &&
			p.Category != "sandbox" && (p.Category != "exec" || boxed)
		switch {
		case p.Host != "":
			// The action lands on a fleet machine, not here: the card names it
			// first. HostName is the machine's label as the broker escaped it;
			// the detail starts with its id and that name too.
			it.Machine = p.Host
			it.Title = `On the machine "` + p.HostName + `": your agent `
			if it.Can != "" {
				it.Title += "wants to " + strings.TrimSuffix(it.Can, " on this computer")
			} else {
				it.Title += "is asking for your OK"
			}
		case it.Pairing:
			it.Title = "Another computer wants to pair with this one"
		case p.Category == "investigate":
			it.Title = "Tenant wants to look into an alert"
		case it.Can != "":
			it.Title = "Your agent wants to " + it.Can
		default:
			it.Title = "Your agent is asking for your OK"
		}
		if p.Origin == "imessage" {
			it.From = "iMessage"
		}
		q.Items = append(q.Items, it)
	}
	return q
}

// sandboxed reports whether commands really stay in the sandbox: the facts
// say they run in it and Leave the sandbox still asks or denies. Allowed, or
// granted for the session, a command the box refuses re-runs outside it
// unasked. Fail-closed: no sandbox category, not sandboxed.
func sandboxed(facts SafetyFacts, perms []control.PermissionInfo) bool {
	i := slices.IndexFunc(perms, func(p control.PermissionInfo) bool { return p.Category == "sandbox" })
	return facts.Sandboxed && i >= 0 && perms[i].Mode != "allow" && !perms[i].Session
}

func (s *Server) facts() SafetyFacts {
	if s.safetyFacts == nil {
		return SafetyFacts{}
	}
	return s.safetyFacts()
}

// renderQueue renders one approvalq.html define: "approval-queue" (what
// /events patches) or "approvalq" (the queue and #approval-note).
func (s *Server) renderQueue(name string, q approvalQueue) (string, error) {
	var buf bytes.Buffer
	if err := s.tmpl.approvalQ.ExecuteTemplate(&buf, name, q); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// patchApprovals writes the whole #approval-queue as one outer patch and
// flushes. It reports false once the client has gone. A render error skips
// this patch; the next event or resync tries again.
func (s *Server) patchApprovals(ctx context.Context, w http.ResponseWriter) bool {
	part, err := s.renderQueue("approval-queue", s.queue())
	if err != nil {
		RenderFailed.Log(ctx, "page", "approval-queue", "err", err)
		return true
	}
	if _, err := w.Write(patchElements("", "", part)); err != nil {
		return false
	}
	flush(w)
	return true
}

// queueResync is how often /events re-renders the queue with no event: it
// catches up past the lossy event broker and refreshes "waiting N min".
var queueResync = 15 * time.Second

type safetyPageData struct {
	layoutData
	Configured bool // either control: the page isn't wholly off
	Queue      approvalQueue
	HasModes   bool
	Modes      []ModeRow
	New        []ModeRow
	NewHead    string
	Msg, Err   string
}

func (s *Server) handleSafetyPage(w http.ResponseWriter, r *http.Request) {
	d := s.safetyData()
	d.Queue, d.Msg, d.Err = s.queue(), r.URL.Query().Get("msg"), r.URL.Query().Get("err")
	s.render(w, s.tmpl.safety, d)
}

// safetyData is the modes as the broker says now: no queue, no flash.
func (s *Server) safetyData() safetyPageData {
	d := safetyPageData{
		layoutData: layoutData{Title: "Approvals & safety", Page: "safety"},
		Configured: s.safety != nil || s.approvals != nil,
		HasModes:   s.safety != nil,
	}
	d.Modes = s.modeRows()
	for _, m := range d.Modes {
		if m.New {
			d.New = append(d.New, m)
		}
	}
	switch n := len(d.New); n {
	case 0:
	case 1:
		d.NewHead = "An update added a new safety check. Your agent asks you first until you choose."
	default:
		d.NewHead = "An update added " + checks(n) + ". Your agent asks you first until you choose."
	}
	return d
}

// safetyDone answers a mode form: in place for a Datastar post, with
// #safety-modes re-rendered; a 303 with the flash otherwise.
func (s *Server) safetyDone(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	s.formDone(w, r, "/safety", msg, errMsg, func() (string, error) {
		return part(s.tmpl.safety, "safety-modes", s.safetyData())
	})
}

// modeRows is one row per category of the main broker, with the consequence
// of its current mode and of Allow, in plain words from the facts.
func (s *Server) modeRows() []ModeRow {
	if s.safety == nil {
		return nil
	}
	discord := map[string]string{}
	if s.access != nil {
		if v := s.access.View(); v.RelayRunning {
			for _, p := range v.DiscordPerms {
				discord[p.Category] = map[string]string{"ask": "asks first", "allow": "allowed without asking", "deny": "not allowed"}[p.Mode]
			}
		}
	}
	facts, perms := s.facts(), s.safety.Permissions()
	boxed := sandboxed(facts, perms)
	var rows []ModeRow
	for _, p := range perms {
		row := ModeRow{Category: p.Category, Title: p.Title, Can: p.Can, Mode: p.Mode, New: p.New, Session: p.Session, Discord: discord[p.Category]}
		// Outside the sandbox a command can reach Tenant's own files (F9):
		// said on Allow, and on a grant or Allow already on (the backend can
		// change under it while Tenant runs).
		risk := ""
		if p.Category == "sandbox" || p.Category == "exec" && !boxed {
			risk = " Commands run as " + facts.runsAs() + ". With this on, your agent could also change its own settings and answer its own requests on this page."
		}
		switch {
		case p.Session:
			row.Consequence = "Allowed without asking until Tenant restarts." + risk
		case p.Mode == "allow":
			row.Consequence = "Can " + p.Can + " without asking you." + risk
		case p.Mode == "deny":
			row.Consequence = "Can't " + p.Can + ". It won't ask."
		default:
			row.Consequence = "Asks you before it can " + p.Can + "."
		}
		row.Allow = "Let your agent " + p.Can + " without asking you?" + risk
		if p.Category == "exec" && boxed {
			row.Allow += " Commands run inside the sandbox."
		}
		rows = append(rows, row)
	}
	return rows
}

// runsAs names the account commands run as, and what that means.
func (f SafetyFacts) runsAs() string {
	switch {
	case f.System:
		return "SYSTEM, which has full control of this Windows computer"
	case f.Account != "":
		return f.Account + ", with everything that account can do"
	}
	return "the account Tenant runs under, with everything that account can do"
}

// permission finds category among the broker's categories.
func (s *Server) permission(category string) (control.PermissionInfo, bool) {
	for _, p := range s.safety.Permissions() {
		if p.Category == category {
			return p, true
		}
	}
	return control.PermissionInfo{}, false
}

// handleSafetyModeForm is POST /safety/mode (category, mode): the same path
// as the terminal's /permissions, recorded with surface=dashboard. Loosening
// passed needConfirm; "Ask me again" is mode=ask, which also ends a session
// grant.
func (s *Server) handleSafetyModeForm(w http.ResponseWriter, r *http.Request) {
	if s.safety == nil {
		s.safetyDone(w, r, "", modesOff)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	p, ok := s.permission(r.FormValue("category"))
	switch {
	case !ok:
		s.safetyDone(w, r, "", "That safety check doesn't exist. Nothing changed.")
		return
	case mode != "ask" && mode != "allow" && mode != "deny":
		s.safetyDone(w, r, "", "Choose Ask, Allow or Deny. Nothing changed.")
		return
	}
	if _, err := s.safety.SetPermissionFrom("dashboard", p.Category, mode); err != nil {
		s.safetyDone(w, r, "", s.flashFor(r.Context(), "safety mode", err, "Couldn't change "+p.Title+". Nothing changed."))
		return
	}
	msg := map[string]string{
		"ask":   p.Title + ": your agent asks you first.",
		"allow": p.Title + ": your agent goes ahead without asking.",
		"deny":  p.Title + ": your agent can't do this now.",
	}[mode]
	if p.Session && mode == "ask" {
		msg = p.Title + ": your agent asks you again from now on."
	}
	s.safetyDone(w, r, msg, "")
}

// handleSafetyKeepNewForm is POST /safety/new, "Keep them at Ask": an
// explicit, recorded choice for every New category, which clears the mark.
func (s *Server) handleSafetyKeepNewForm(w http.ResponseWriter, r *http.Request) {
	if s.safety == nil {
		s.safetyDone(w, r, "", modesOff)
		return
	}
	var kept []string
	for _, p := range s.safety.Permissions() {
		if !p.New {
			continue
		}
		if _, err := s.safety.SetPermissionFrom("dashboard", p.Category, "ask"); err != nil {
			s.safetyDone(w, r, "", s.flashFor(r.Context(), "safety keep new", err, "Couldn't keep "+p.Title+" at Ask. Try again, or choose it below."))
			return
		}
		kept = append(kept, p.Title)
	}
	if len(kept) == 0 {
		s.safetyDone(w, r, "There are no new safety checks to review.", "")
		return
	}
	s.safetyDone(w, r, "Kept at Ask: "+strings.Join(kept, ", ")+". Your agent asks you first.", "")
}

// handleSafetyDecide is POST /safety/approvals/{id}?decision=…: one answer to
// one request. First answer wins (the registry); a loser, or a choice the
// item doesn't offer, changes nothing. A Datastar request gets the queue and
// #approval-note patched in place; anything else a redirect to the page.
func (s *Server) handleSafetyDecide(w http.ResponseWriter, r *http.Request) {
	id, decision := r.PathValue("id"), r.FormValue("decision")
	var msg, errMsg string
	it, found := ApprovalItem{}, false
	if s.approvals != nil {
		for _, x := range s.queue().Items {
			if x.ID == id {
				it, found = x, true
			}
		}
	}
	switch done, ok := decisionDone[decision]; {
	case s.approvals == nil:
		errMsg = approvalsOff
	case !ok, decision == "approve_session" && found && !it.Standing:
		errMsg = notAvailable
	case !found:
		errMsg = alreadyAnswered
	default:
		if err := s.approvals.Decide(id, decision); err != nil {
			errMsg = s.flashFor(r.Context(), "approval decide", err, alreadyAnswered)
		} else {
			msg = done
		}
	}
	if r.Header.Get("Datastar-Request") == "" {
		redirectFlash(w, r, "/safety", msg, errMsg)
		return
	}
	q := s.queue()
	q.Msg, q.Err = msg, errMsg
	part, err := s.renderQueue("approvalq", q)
	if err != nil {
		RenderFailed.Log(r.Context(), "page", "approvalq", "err", err)
		redirectFlash(w, r, "/safety", msg, errMsg)
		return
	}
	_ = writeDatastarPatch(w, part)
}
