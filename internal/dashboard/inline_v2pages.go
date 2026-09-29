package dashboard

// inline_v2pages.go holds what Helpers, Research and the Model page's
// server add share for their forms that answer in place (DESIGN.md "Forms
// that don't reload"): the answer to a create.

import "net/http"

// createDone is formDone for a create that worked (a helper added, research
// started): the region is swapped in whole instead of morphed, because a
// morph keeps what was typed and the next one should start empty.
func (s *Server) createDone(w http.ResponseWriter, r *http.Request, page, msg string, region func() (string, error)) {
	s.createReply(w, r, page, msg, "", region)
}

// createWarn is createDone for a create that worked but needs the
// operator's eye (a model server added whose tool calls weren't confirmed):
// the toast is an alert, and the region still comes back whole.
func (s *Server) createWarn(w http.ResponseWriter, r *http.Request, page, warn string, region func() (string, error)) {
	s.createReply(w, r, page, "", warn, region)
}

func (s *Server) createReply(w http.ResponseWriter, r *http.Request, page, msg, warn string, region func() (string, error)) {
	s.formReply(w, r, page, msg, warn, func() (string, error) { return "", nil })
	if r.Header.Get("Datastar-Request") == "" {
		return
	}
	body, err := region()
	if err != nil {
		RenderFailed.Log(r.Context(), "page", page, "err", err)
		return
	}
	if body != "" {
		_, _ = w.Write(patchElements("", "replace", body))
		flush(w)
	}
}
