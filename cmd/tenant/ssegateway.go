package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guardSSEGateway is everything the `mcp-memory --sse-addr` gateway asks of a
// request before the MCP transport sees it. A loopback bind keeps other
// machines out, but not the other programs on this one, and not a browser on
// this one: a page from anywhere can send requests to 127.0.0.1. Three checks,
// the same three the dashboard makes (internal/dashboard/auth.go), in the
// same order:
//
//   - Host, on a loopback bind. The request must name the gateway as
//     localhost or by a loopback IP address, on the gateway's own port. A
//     page reaches a local service under a name of its own choosing by
//     re-pointing that name's DNS at 127.0.0.1 (rebinding), and then the Host
//     header carries that name; an IP address cannot be re-pointed. A
//     non-loopback bind (--insecure-lan, the operator's explicit opt-out for
//     an overlay network) is reached by whatever name that network gives the
//     machine, so its Host is not checked.
//   - Origin. A browser says which site made a request; a client that is not
//     a browser sends no Origin. A request that carries one is refused unless
//     it is the gateway's own address, which no page is served from.
//   - Token. Every request, the stream and each message, carries
//     `Authorization: Bearer <token>`, the token in gateway-auth-token
//     (gatewayauth.go), compared in constant time. Without it the gateway
//     answers 401 and nothing reaches the transport. An empty token is never
//     accepted: a gateway built without one refuses everything.
//
// The transport adds two more (a session id on every message, and
// application/json only; internal/mcp/transport/sse.go).
func guardSSEGateway(addr, token string, next http.Handler) http.Handler {
	bindHost, bindPort, err := net.SplitHostPort(addr)
	if err != nil {
		bindHost, bindPort = addr, ""
	}
	loopbackOnly := isLoopbackAddr(bindHost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loopbackOnly && !loopbackGatewayHost(r.Host, bindPort) {
			http.Error(w, "unrecognized gateway host: use 127.0.0.1 or localhost", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !gatewayOriginAllowed(origin, r.Host) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if !gatewayTokenMatches(r.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or invalid bearer token: send the gateway's token as \"Authorization: Bearer <token>\"", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gatewayTokenMatches reports whether an Authorization header carries the
// gateway's token. Only the header: a token in a URL ends up in histories and
// logs.
func gatewayTokenMatches(header, token string) bool {
	got, ok := strings.CutPrefix(header, "Bearer ")
	return ok && got != "" && token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// loopbackGatewayHost reports whether a Host header names a loopback gateway
// itself: localhost or a loopback IP address, on the gateway's port. bindPort
// "" or "0" (an OS-assigned port) accepts any port. Any other name is the one
// thing a page can point where it likes.
func loopbackGatewayHost(hostHeader, bindPort string) bool {
	host, port, err := net.SplitHostPort(hostHeader)
	if err != nil { // no port: the default one, which the gateway is not on unless bindPort is 80
		host, port = hostHeader, "80"
	}
	if bindPort != "" && bindPort != "0" && port != bindPort {
		return false
	}
	return isLoopbackAddr(strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")))
}

// gatewayOriginAllowed reports whether an Origin header is the gateway's own
// address: http, the same host and port the request named, nothing else.
func gatewayOriginAllowed(origin, hostHeader string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.EqualFold(u.Host, hostHeader)
}
