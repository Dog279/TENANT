package mcpremote

// CloudFront-fronted MCP servers (notably Atlassian's mcp.atlassian.com) serve
// OAuth Authorization-Server metadata whose "issuer" is a sibling/sub host
// (e.g. cf.mcp.atlassian.com) of the URL the client fetched it from
// (mcp.atlassian.com). RFC 8414 §3.3 requires issuer equality, and the official
// go-sdk enforces it strictly — so the connect fails before the browser ever
// opens, even though the actual authorize/token/register endpoints in the same
// document are valid Atlassian URLs.
//
// issuerRewriteTransport is a narrowly-scoped workaround: on Authorization-
// Server-metadata responses ONLY, it rewrites the "issuer" string to the origin
// the client requested — and ONLY when the declared issuer is host-related to
// that origin (one is a sub/parent domain of the other). An unrelated issuer
// (a redirect to evil.com) is left untouched, so the SDK still rejects it. No
// endpoints are altered; only the issuer label is reconciled.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type issuerRewriteTransport struct{ base http.RoundTripper }

func newRewritingClient() *http.Client {
	return &http.Client{Transport: issuerRewriteTransport{base: http.DefaultTransport}}
}

func (t issuerRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || !isASMetadataPath(req.URL.Path) {
		return resp, err
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	want := req.URL.Scheme + "://" + req.URL.Host
	if nb, changed := rewriteASIssuer(body, want); changed {
		body = nb
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func isASMetadataPath(p string) bool {
	return strings.Contains(p, "/.well-known/oauth-authorization-server") ||
		strings.Contains(p, "/.well-known/openid-configuration")
}

// rewriteASIssuer reconciles the "issuer" field of an AS-metadata JSON body to
// wantIssuer, but only when the existing issuer is host-related to wantIssuer
// (the CDN-fronting case). Returns the (possibly unchanged) body + whether it
// changed. Any parse failure or unrelated issuer leaves the body untouched.
func rewriteASIssuer(body []byte, wantIssuer string) ([]byte, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	iss, ok := m["issuer"].(string)
	if !ok || iss == wantIssuer || !issuerHostRelated(iss, wantIssuer) {
		return body, false
	}
	m["issuer"] = wantIssuer
	nb, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return nb, true
}

// issuerHostRelated reports whether two issuer URLs share a domain lineage: equal
// hosts, or one a subdomain of the other (cf.mcp.atlassian.com ~ mcp.atlassian.com).
// Unrelated hosts (evil.com vs atlassian.com) are NOT related.
func issuerHostRelated(issuer, want string) bool {
	iu, e1 := url.Parse(issuer)
	wu, e2 := url.Parse(want)
	if e1 != nil || e2 != nil {
		return false
	}
	a, b := iu.Hostname(), wu.Hostname()
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}
