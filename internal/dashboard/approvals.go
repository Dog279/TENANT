package dashboard

// approvals.go is the TEN-194 headless approval surface. When tenant runs as a
// daemon (`tenant serve`) there is no interactive TUI to prompt for a dangerous
// action, so the approval broker's requests are drained into an ApprovalControl
// and the operator resolves them from the dashboard (or a future attach client)
// instead. nil control ⇒ the routes report empty/"not configured" — the
// interactive TUI keeps draining its own prompts and never sets this.

import (
	"net/http"
)

// ApprovalControl is the dashboard's view onto the headless approval queue.
// The cmd/tenant side implements it over the live approval broker.
type ApprovalControl interface {
	// Pending lists the dangerous-action requests currently awaiting a decision.
	Pending() []PendingApproval
	// Decide resolves request id with one of: "approve", "approve_session",
	// "approve_always", "deny". An unknown id (already resolved or expired) or an
	// unknown decision string returns an error.
	Decide(id, decision string) error
}

// PendingApproval is one awaiting dangerous-action request as the REST API
// renders it. AgeSecs lets the UI (and the doctor probe) flag a request that
// has been waiting too long — the classic headless wedge symptom.
type PendingApproval struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Action   string `json:"action"`
	Detail   string `json:"detail"`
	AgeSecs  int    `json:"age_secs"`
}

// SetApprovals installs the headless approval surface after construction
// (mirrors SetCron). Safe to leave nil for the interactive TUI.
func (s *Server) SetApprovals(a ApprovalControl) { s.approvals = a }

// handleApprovals — GET /api/approvals: the pending dangerous-action queue.
// Returns an empty list (not an error) when no control is wired, so a poller
// gets a stable shape regardless of mode.
func (s *Server) handleApprovals(w http.ResponseWriter, _ *http.Request) {
	if s.approvals == nil {
		writeJSON(w, http.StatusOK, []PendingApproval{})
		return
	}
	pending := s.approvals.Pending()
	if pending == nil {
		pending = []PendingApproval{}
	}
	writeJSON(w, http.StatusOK, pending)
}

type approvalDecisionRequest struct {
	Decision string `json:"decision"`
}

// handleApprovalDecide — POST /api/approvals/{id} {"decision":"approve"|…}:
// resolve one pending request. The decision unblocks the gated turn goroutine.
func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "approvals not configured")
		return
	}
	id := r.PathValue("id")
	var req approvalDecisionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.approvals.Decide(id, req.Decision); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
