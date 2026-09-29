package dashboard

// inline_v2pages.go holds what Helpers and Research share for their forms
// that answer in place (DESIGN.md "Forms that don't reload"): the answer to
// a create.

import "net/http"

// createDone is formDone for a create that worked (a helper added, research
// started): the region is swapped in whole instead of morphed, because a
// morph keeps what was typed and the next one should start empty.
func (s *Server) createDone(w http.ResponseWriter, r *http.Request, page, msg string, region func() (string, error)) {
	s.formDone(w, r, page, msg, "", func() (string, error) { return "", nil })
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
