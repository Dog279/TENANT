package dashboard

// safety.go is the "Approvals & safety" page (TEN-207, TEN-343): the live
// queue of requests waiting for the operator's OK and the per-category modes
// of the main broker. The foundation lays the contract (control, facts,
// views, route); WP-safety builds the page on it.

import (
	"net/http"

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
	Sandboxed bool   // the exec profile for dashboard-driven turns is restricted and enforced
}

// ApprovalItem is one queued request, render-ready.
type ApprovalItem struct {
	ID, Category, Title, Action, Detail string
	From                                string // "" = this computer, "iMessage"; none for pairing
	Waited                              string // waited(AgeSecs)
	Can                                 string
	Lines                               int  // Detail line count; > 8 adds "This request is N lines long. Read all of it before you allow it."
	Standing                            bool // SessionOK && Origin=="local" && !(cat ∈ {exec,sandbox} && !facts.Sandboxed)
	Pairing                             bool
	Actionable                          bool // ID matches ^[A-Za-z0-9-]+$
}

// ModeRow is one category's mode row, render-ready.
type ModeRow struct {
	Category, Title, Can, Mode, Consequence string
	New, Session                            bool
	Discord                                 string // relay mode for this category, "" when the relay isn't running
}

// SetSafety installs the broker and the facts about it (facts may be nil).
func (s *Server) SetSafety(c SafetyControl, facts func() SafetyFacts) {
	s.safety, s.safetyFacts = c, facts
}

func (s *Server) mountSafetySSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /safety", s.handleSafetyPage)
}

type safetyPageData struct {
	layoutData
	Configured bool
	Msg, Err   string
}

func (s *Server) handleSafetyPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.safety, safetyPageData{
		layoutData: layoutData{Title: "Approvals & safety", Page: "safety"},
		Configured: s.safety != nil,
		Msg:        r.URL.Query().Get("msg"),
		Err:        r.URL.Query().Get("err"),
	})
}
