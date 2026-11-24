package dashboard

// lan.go is the dashboard served a second time, to the local network
// (TEN-544): for a site that cannot run Tailscale or any VPN. It is off until
// the operator turns it on, and it is a second listener, never a moved one:
// the loopback listener stays as it is, so the steps taken "at this computer"
// (atThisComputer) and `tailscale serve` keep working.
//
// The pages, the sign-in and every check are the loopback listener's
// (served). What differs on this listener:
//
//   - TLS always, with the certificate the caller hands over. The sign-in key
//     and the session cookie never cross the network in the clear.
//   - Where a request may come from: this computer, a private or link-local
//     address, or a network the operator listed. Anything else is refused
//     before sign-in is looked at.
//   - The Host it answers to. The listener may be bound to every interface, so
//     there is no one address to compare with: a request must name the address
//     it actually arrived at, or one of this computer's names. A page on
//     another site that points its own name at this computer (DNS rebinding)
//     names neither.

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"

	"tenant/internal/logs/event"
)

// LAN is how the dashboard is served to the local network.
type LAN struct {
	// Cert is what the listener answers with. It is required: this listener
	// speaks nothing but TLS.
	Cert tls.Certificate
	// Names are the host names this computer answers to here, in lower case,
	// besides the address a request arrived at: its own name, as the other
	// computers on the network know it.
	Names []string
	// Allow are the networks a request may come from besides this computer
	// and the private and link-local ranges.
	Allow []netip.Prefix
}

// lanKey marks a request that arrived on the LAN listener, with its LAN.
type lanKey struct{}

// lanOf is the LAN listener's settings for a request that arrived on it, nil
// for one that arrived on the dashboard's own listener.
func lanOf(r *http.Request) *LAN {
	l, _ := r.Context().Value(lanKey{}).(*LAN)
	return l
}

// overTLS reports whether r arrived encrypted on this server's own listener:
// the dashboard's when it has a certificate, or the LAN listener always.
func (s *Server) overTLS(r *http.Request) bool {
	return r.TLS != nil || s.cfg.TLSCert != "" && s.cfg.TLSKey != ""
}

// from reports whether a request from remote (ip:port) may use the listener.
func (l *LAN) from(remote string) bool {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap().WithZone("")
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	return slices.ContainsFunc(l.Allow, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// lanHost is checkHost for the LAN listener: r must name the address and port
// it arrived at, or one of this computer's names (l.Names, localhost on the
// loopback interface, or a name the operator trusts for the dashboard) on that
// port.
func (s *Server) lanHost(r *http.Request, l *LAN) bool {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	host, port, ok := splitAuthority(r.Host)
	if !ok || local == nil {
		return false
	}
	if port == "" {
		port = "443" // what a browser leaves out of an https address
	}
	if port != strconv.Itoa(local.Port) {
		return false
	}
	at, _ := netip.AddrFromSlice(local.IP)
	at = at.Unmap().WithZone("")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().WithZone("") == at
	}
	if host == "localhost" {
		return at.IsLoopback()
	}
	if slices.Contains(l.Names, host) {
		return true
	}
	s.trustedMu.RLock()
	defer s.trustedMu.RUnlock()
	for _, allowed := range append(slices.Clone(s.cfg.AllowedHosts), s.trusted...) {
		if h, _, ok := splitAuthority(allowed); ok && h == host {
			return true
		}
	}
	return false
}

// ServeLAN serves the dashboard on ln for the local network, over TLS, until
// ctx is cancelled. It is the same handler as Serve's, so a browser signed in
// here sees what one on this computer sees, but for what stays at this
// computer.
func (s *Server) ServeLAN(ctx context.Context, ln net.Listener, lan LAN) (err error) {
	defer ln.Close()
	ctx, act := event.Start(ctx, Serving, "addr", ln.Addr().String(), "tls", true)
	defer act.End(&err)
	switch {
	case s.cfg.Auth == "":
		return errors.New("dashboard requires an auth token")
	case len(lan.Cert.Certificate) == 0:
		return errors.New("the dashboard is served to the local network over TLS only, and there is no certificate")
	}
	srv := newDashboardHTTPServer(ln.Addr().String(), s.served(), &tls.Config{Certificates: []tls.Certificate{lan.Cert}, MinVersion: tls.VersionTLS12})
	srv.ErrorLog = log.New(serverErrors{ctx}, "", 0)
	srv.BaseContext = func(net.Listener) context.Context { return context.WithValue(context.Background(), lanKey{}, &lan) }
	return serveUntil(ctx, srv, ln, true)
}
