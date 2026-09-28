package dashboard

// ui.go is TEN-211's shared page kit: the confirm step (a native <details>, no
// JS) and the server gate behind it, the flash redirect, the plain-error rule
// for control errors, the text helpers the templates call, and the global
// status strip every page polls (#ui-status). The partials live in
// templates/ui.html; DESIGN.md says when to use each.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"tenant/internal/agent"
)

// ConfirmView is the ui-confirm partial's input: a dangerous action behind a
// second click. Yes is "Yes, " + Label with its first letter lowercased.
type ConfirmView struct {
	Action, Label, Yes, Consequence string
	Hidden                          []HiddenField
}

// HiddenField is one form field the confirmed POST carries.
type HiddenField struct{ Name, Value string }

// uiFuncs is parsed into every template set (parseSSR and logsTmpl).
var uiFuncs = template.FuncMap{"confirm": confirmStep, "visibleText": visibleText, "waited": waited}

// confirmStep builds a ConfirmView: the action, the first button's label, one
// plain consequence sentence, then name/value pairs for the hidden fields. An
// odd number of pairs is an error, so the page fails to render in tests.
func confirmStep(action, label, consequence string, kv ...string) (ConfirmView, error) {
	if len(kv)%2 != 0 {
		return ConfirmView{}, fmt.Errorf("confirm %s: odd number of hidden field arguments", action)
	}
	v := ConfirmView{Action: action, Label: label, Consequence: consequence, Yes: "Yes, " + lowerFirst(label)}
	for i := 0; i < len(kv); i += 2 {
		v.Hidden = append(v.Hidden, HiddenField{Name: kv[i], Value: kv[i+1]})
	}
	return v, nil
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}

// visibleText shows what a request really says (F10): bidi and zero-width
// controls, and other control characters, render as "⟨U+XXXX⟩" so text can't
// hide or reorder itself, and a run of 3 or more blank lines collapses to one
// "⟨N blank lines⟩" line so it can't push the rest out of view.
func visibleText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		if hiddenRune(r) {
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
			continue
		}
		b.WriteRune(r)
	}
	lines := strings.Split(b.String(), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		switch n := j - i; {
		case n >= 3:
			out = append(out, fmt.Sprintf("⟨%d blank lines⟩", n))
			i = j
		case n > 0:
			out = append(out, lines[i:j]...)
			i = j
		default:
			out = append(out, lines[i])
			i++
		}
	}
	return strings.Join(out, "\n")
}

func hiddenRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r):
		return true
	}
	switch r {
	case '\u200B', '\u200C', '\u200D', '\u2028', '\u2029', '\u2060', '\uFEFF':
		return true
	}
	return false
}

// waited says how long a request has waited, in words.
func waited(secs int) string {
	switch {
	case secs < 60:
		return "waiting less than a minute"
	case secs < 3600:
		return "waiting " + strconv.Itoa(secs/60) + " min"
	}
	return "waiting " + strconv.Itoa(secs/3600) + " h"
}

// notConfirmed is the 428 body for a dangerous POST without its confirm step.
const notConfirmed = "Nothing changed: that needs a second click to confirm."

// needConfirm wraps a dangerous route (D2): when when(r) says it needs
// confirming (nil = always), a request without confirm=yes gets 428 and h
// never runs. The ui-confirm partial sends the field. This is friction, not
// security: checkCSRF and checkOrigin are the boundary.
func needConfirm(when func(*http.Request) bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if (when == nil || when(r)) && r.FormValue("confirm") != "yes" {
			http.Error(w, notConfirmed, http.StatusPreconditionRequired)
			return
		}
		h(w, r)
	}
}

// confirmUnless is needConfirm's usual condition, fail-closed: confirm is
// needed unless the trimmed, lowercased form value key is one of safe. So
// "ALLOW", " allow", "on", "always" or "1" can never skip the step, whatever
// the handler's own parser would accept.
func confirmUnless(key string, safe ...string) func(*http.Request) bool {
	lower := make([]string, len(safe))
	for i, v := range safe {
		lower[i] = strings.ToLower(v)
	}
	return func(r *http.Request) bool {
		return !slices.Contains(lower, strings.ToLower(strings.TrimSpace(r.FormValue(key))))
	}
}

// redirectFlash is POST/redirect/GET with one flash: ?err= when errMsg is
// set, else ?msg= when msg is.
func redirectFlash(w http.ResponseWriter, r *http.Request, page, msg, errMsg string) {
	sep := "?"
	if strings.Contains(page, "?") {
		sep = "&"
	}
	switch {
	case errMsg != "":
		page += sep + "err=" + url.QueryEscape(errMsg)
	case msg != "":
		page += sep + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, page, http.StatusSeeOther)
}

// PlainError is an error whose text is fit for the page (D21): an adapter
// wraps the failures it can phrase plainly ("A helper named X already
// exists."). Any other control error stays off the page.
type PlainError string

func (e PlainError) Error() string { return string(e) }

// flashFor is the text a failed action shows: err's own text when it is a
// PlainError; otherwise fallback, the action's fixed plain message, and the
// raw error goes to the log (ActionFailed).
func (s *Server) flashFor(ctx context.Context, action string, err error, fallback string) string {
	var pe PlainError
	if errors.As(err, &pe) {
		return string(pe)
	}
	ActionFailed.Log(ctx, "action", action, "err", err)
	return fallback
}

// StatusView is the #ui-status strip: what the agent is doing, what waits
// for the operator, and what an update changed. The zero value renders as an
// empty strip, which CSS hides.
type StatusView struct {
	Busy      *BusyView
	Approvals int          // len(Pending()), 0 when no approval control is wired
	Notices   []NoticeView // main broker New → /safety; Access Discord/iMessage New → /access
}

// BusyView is one "agent busy" line (ui-busy).
type BusyView struct {
	Title, Since, Detail string
	Stop, StopLabel      string // POST target and "Stop"/"Cancel"; "" = none
	Answer               bool   // approvals > 0: show the "Answer it" link
}

// NoticeView is one notice line with its link.
type NoticeView struct{ Text, Href, Link string }

// statusView reads the strip's state from the controls, each nil-safe.
func (s *Server) statusView() StatusView {
	var v StatusView
	if s.approvals != nil {
		v.Approvals = len(s.approvals.Pending())
	}
	v.Busy = s.busyView(v.Approvals > 0)
	if s.safety != nil {
		var titles []string
		for _, p := range s.safety.Permissions() {
			if p.New {
				titles = append(titles, p.Title)
			}
		}
		if n := len(titles); n > 0 {
			v.Notices = append(v.Notices, NoticeView{
				Text: "An update added " + checks(n) + ": " + strings.Join(titles, ", ") + ". Your agent asks you first until you choose.",
				Href: "/safety", Link: "Review",
			})
		}
	}
	if s.access != nil {
		av := s.access.View()
		for _, ch := range []struct {
			name  string
			perms []PermissionRow
		}{{"Discord", av.DiscordPerms}, {"iMessage", av.IMessagePerms}} {
			n := 0
			for _, p := range ch.perms {
				if p.New {
					n++
				}
			}
			if n > 0 {
				verb := " ask"
				if n == 1 {
					verb = " asks"
				}
				v.Notices = append(v.Notices, NoticeView{
					Text: ch.name + " conversations: " + checks(n) + verb + " you first until you choose.",
					Href: "/access", Link: "Review",
				})
			}
		}
	}
	return v
}

func checks(n int) string {
	if n == 1 {
		return "1 new safety check"
	}
	return strconv.Itoa(n) + " new safety checks"
}

// busyView is the strip's busy line: this dashboard's own chat, a request
// queued behind the serve turn gate, or the gate busy with someone else. The
// gate is read by duck typing, as /api/status does; in the terminal app the
// runner is the bare agent and only the dashboard's own chat shows.
func (s *Server) busyView(approvals bool) *BusyView {
	mine, age := s.coord.busy()
	var gate bool
	var gateAge time.Duration
	if lr, ok := s.agent.(liveness); ok {
		gate, gateAge = lr.ActiveTurn()
	}
	var q agent.TurnQueueStatus
	if qr, ok := s.agent.(queueLiveness); ok {
		q = qr.TurnQueueStatus()
	}
	var queued *agent.TurnQueueEntry
	for i := range q.Entries {
		if q.Entries[i].Source == agent.RouteSurfaceDashboard {
			queued = &q.Entries[i]
			break
		}
	}
	switch {
	case mine && queued != nil:
		b := &BusyView{Title: "Your agent is busy with " + busyWith(q.ActiveSource), Since: shortDur(gateAge), Stop: "/chat/stop", StopLabel: "Cancel"}
		b.Detail = "Your message is #" + strconv.Itoa(queued.Position) + " in line, about " + shortDur(queued.ETA) + "."
		if queued.ETA <= 0 {
			b.Detail = "Your message is next. It starts when the current request finishes."
		}
		return b
	case mine:
		b := &BusyView{Title: "Your agent is answering a message", Since: shortDur(age), Detail: "New messages wait until it's done.", Stop: "/chat/stop", StopLabel: "Stop"}
		if approvals {
			b.Detail, b.Answer = "It's paused until you answer a request.", true
		}
		return b
	case gate:
		return &BusyView{Title: "Your agent is busy with " + busyWith(q.ActiveSource), Since: shortDur(gateAge)}
	}
	return nil
}

// busyWith names the turn gate's active source in plain words. Discord and
// iMessage conversations share the relay source, and alert checks run as
// scheduled jobs, so each pair is named together.
func busyWith(source string) string {
	switch agent.RouteSurface(source) {
	case agent.RouteSurfaceRelay:
		return "a Discord or iMessage conversation"
	case agent.RouteSurfaceCron:
		return "a scheduled job or an alert"
	case agent.RouteSurfaceTUI:
		return "a message from the terminal"
	}
	return "another request"
}

// uiPart renders one ui.html partial on its own (an SSE patch).
func (s *Server) uiPart(name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := s.tmpl.ui.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// handleUIStatus is GET /ui/status: one patch of #ui-status, which layout.html
// polls on every page (D13). A render error is a 204: the strip keeps what it
// showed.
func (s *Server) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	part, err := s.uiPart("ui-status", s.statusView())
	if err != nil {
		RenderFailed.Log(r.Context(), "page", "ui-status", "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = writeDatastarPatch(w, part)
}

// served is the production handler chain: request records around the
// security envelope around the routed mux. Serve and the browser tests both
// use it, so a test sees what the operator's browser sees.
func (s *Server) served() http.Handler { return s.observe(s.secure(s.Handler())) }
