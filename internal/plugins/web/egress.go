package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ipResolver is the narrow DNS surface used by the egress guard. Keeping it
// injectable makes DNS and rebinding behavior deterministic in tests.
type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type egressGuard struct {
	resolver ipResolver
	allow    map[string]struct{}
	dial     func(context.Context, string, string) (net.Conn, error)
}

func newEgressGuard(cfg Config) *egressGuard {
	resolver := cfg.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dial := cfg.dialContext
	if dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	allow := make(map[string]struct{}, len(cfg.PrivateNetwork))
	for _, host := range cfg.PrivateNetwork {
		host = normalizeHostname(host)
		if host != "" && !isNumericHost(host) {
			allow[host] = struct{}{}
		}
	}
	return &egressGuard{resolver: resolver, allow: allow, dial: dial}
}

func normalizeHostname(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func (g *egressGuard) privateAllowed(host string) bool {
	_, ok := g.allow[normalizeHostname(host)]
	return ok
}

// validateURLSyntax rejects non-network URLs and ambiguous host syntax before
// either Chrome or net/http sees it. Direct numeric targets are classified here
// as well, so legacy browser encodings cannot slip through a DNS parser.
func validateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("blocked: only http/https URLs allowed (got %q)", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("blocked: url must include a host")
	}
	if _, err := strconv.Atoi(u.Port()); u.Port() != "" && err != nil {
		return "", errors.New("blocked: url has an invalid port")
	}
	if addr, numeric, err := parseNumericHost(u.Hostname()); err != nil {
		return "", fmt.Errorf("blocked: invalid numeric host %q", u.Hostname())
	} else if numeric && isBlockedAddr(addr) {
		return "", fmt.Errorf("blocked: private or non-public destination %s", addr)
	}
	return u.String(), nil
}

// ValidateURL resolves every address for the URL and rejects the whole host if
// any answer is non-public. Rejecting mixed public/private answers avoids a
// client selecting a forbidden address after validation.
func (g *egressGuard) ValidateURL(ctx context.Context, raw string) (string, error) {
	clean, err := validateURL(raw)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(clean)
	if _, err := g.resolve(ctx, u.Hostname()); err != nil {
		return "", err
	}
	return clean, nil
}

func (g *egressGuard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, numeric, err := parseNumericHost(host); err != nil {
		return nil, fmt.Errorf("blocked: invalid numeric host %q", host)
	} else if numeric {
		if isBlockedAddr(addr) {
			return nil, fmt.Errorf("blocked: private or non-public destination %s", addr)
		}
		return []netip.Addr{addr}, nil
	}

	addrs, err := g.resolver.LookupNetIP(ctx, "ip", strings.TrimSuffix(host, "."))
	if err != nil {
		return nil, fmt.Errorf("web: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("web: resolve %s: no addresses", host)
	}
	allowedPrivate := g.privateAllowed(host)
	checked := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !addr.IsValid() {
			return nil, fmt.Errorf("blocked: resolver returned an invalid address for %s", host)
		}
		if isBlockedAddr(addr) && !allowedPrivate {
			return nil, fmt.Errorf("blocked: %s resolves to private or non-public destination %s", host, addr)
		}
		checked = append(checked, addr)
	}
	return checked, nil
}

// DialContext resolves once, checks every result, and dials only the checked IP
// literals. The hostname is never handed to the system dialer, which pins the
// decision to this resolution and defeats DNS rebinding between check and use.
func (g *egressGuard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("web: invalid dial address %q: %w", address, err)
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, addr := range addrs {
		conn, dialErr := g.dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		errs = append(errs, dialErr)
	}
	return nil, fmt.Errorf("web: dial %s: %w", host, errors.Join(errs...))
}

// guardedHTTPClient is used by every non-Chrome request. URL checks cover the
// initial request and redirect destinations; the transport's guarded dialer
// repeats the DNS check and pins the connection to the approved IP.
func guardedHTTPClient(g *egressGuard, timeout time.Duration) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = g.DialContext
	base.DialTLSContext = nil
	base.DisableKeepAlives = true
	rt := &guardedRoundTripper{guard: g, base: base}
	return &http.Client{
		Timeout:   timeout,
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			_, err := g.ValidateURL(req.Context(), req.URL.String())
			return err
		},
	}
}

type guardedRoundTripper struct {
	guard *egressGuard
	base  http.RoundTripper
}

func (t *guardedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, err := t.guard.ValidateURL(req.Context(), req.URL.String()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

var blockedMetadataAddrs = map[netip.Addr]struct{}{
	netip.MustParseAddr("100.100.100.200"): {}, // Alibaba Cloud
	netip.MustParseAddr("168.63.129.16"):   {}, // Azure platform virtual IP
	netip.MustParseAddr("192.0.0.192"):     {}, // Oracle Cloud
}

var blockedSpecialPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // shared carrier-grade NAT space
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking networks
}

func isBlockedAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return true
	}
	if _, ok := blockedMetadataAddrs[addr]; ok {
		return true
	}
	for _, prefix := range blockedSpecialPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func isNumericHost(host string) bool {
	_, numeric, _ := parseNumericHost(host)
	return numeric
}

// parseNumericHost accepts both canonical IP literals and the historical
// inet_aton forms browsers support: one to four decimal/octal/hex components.
// This covers 2130706433, 0x7f000001, 017700000001, 127.1, and mixed forms.
func parseNumericHost(host string) (netip.Addr, bool, error) {
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if i := strings.LastIndexByte(host, '%'); i >= 0 { // IPv6 zone
		host = host[:i]
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap(), true, nil
	}
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false, nil
	}
	values := make([]uint64, len(parts))
	for i, part := range parts {
		if part == "" {
			return netip.Addr{}, false, nil
		}
		base := 10
		digits := part
		if len(part) > 2 && (strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X")) {
			base, digits = 16, part[2:]
			if digits == "" || !allDigits(digits, 16) {
				return netip.Addr{}, true, errors.New("invalid hexadecimal component")
			}
		} else if len(part) > 1 && part[0] == '0' {
			base, digits = 8, part[1:]
			if !allDigits(part, 10) {
				return netip.Addr{}, false, nil
			}
		} else if !allDigits(part, 10) {
			return netip.Addr{}, false, nil
		}
		if digits == "" {
			digits = "0"
		}
		v, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			return netip.Addr{}, true, err
		}
		values[i] = v
	}

	var value uint64
	switch len(values) {
	case 1:
		value = values[0]
	case 2:
		if values[0] > 0xff || values[1] > 0xffffff {
			return netip.Addr{}, true, errors.New("numeric component out of range")
		}
		value = values[0]<<24 | values[1]
	case 3:
		if values[0] > 0xff || values[1] > 0xff || values[2] > 0xffff {
			return netip.Addr{}, true, errors.New("numeric component out of range")
		}
		value = values[0]<<24 | values[1]<<16 | values[2]
	case 4:
		for _, v := range values {
			if v > 0xff {
				return netip.Addr{}, true, errors.New("numeric component out of range")
			}
			value = value<<8 | v
		}
	}
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}), true, nil
}

func allDigits(s string, base int) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' && int(r-'0') < base {
			continue
		}
		if base == 16 && ((r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			continue
		}
		return false
	}
	return s != ""
}
