package x

// White-box tests. No live X here and no API keys solicited: every X
// endpoint is an httptest server returning the exact API v2 wire JSON,
// and the OAuth2/PKCE token paths (the security boundary) are decoded
// and asserted against real crypto.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/model"
)

// rewriteDoer sends every request to the test server regardless of the
// hard-coded api.twitter.com host the client builds.
type rewriteDoer struct {
	base *url.URL
	c    *http.Client
}

func (d rewriteDoer) Do(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = d.base.Scheme
	r.URL.Host = d.base.Host
	r.Host = d.base.Host
	return d.c.Do(r)
}

func doerTo(t *testing.T, srv *httptest.Server) rewriteDoer {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return rewriteDoer{base: u, c: srv.Client()}
}

// writeStore drops a PKCE token store on disk and returns its path.
func writeStore(t *testing.T, s tokenStore) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x-token.json")
	b, _ := json.MarshalIndent(s, "", " ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func call(name string, args map[string]any) model.ToolCall {
	b, _ := json.Marshal(args)
	if len(args) == 0 {
		b = []byte(`{}`)
	}
	return model.ToolCall{Name: name, Arguments: b}
}
