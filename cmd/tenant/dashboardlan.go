package main

// dashboardlan.go is serving the dashboard to the local network (TEN-544): a
// second listener beside the loopback one, HTTPS always, off until the
// operator turns it on with /dashboard serve lan, --dashboard-lan or
// dashboard.lan in config.json. The listener and its checks are
// internal/dashboard/lan.go; here are the certificate it answers with and the
// dashboard manager's side of turning it on and off.
//
// It is never turned on from the dashboard itself, by the API key or by a
// tool: only at the keyboard, by a flag, or by the config file.

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/logs/catalog"
	"tenant/internal/secrets"
)

const (
	// defaultDashboardLANAddr is where the LAN listener binds when no address
	// is given: every interface, on its own port. Its own port, because the
	// dashboard's is taken by the loopback listener, which speaks plain HTTP.
	defaultDashboardLANAddr = "0.0.0.0:9771"

	dashboardLANCertFile = "dashboard-lan-cert.pem"
	dashboardLANKeyFile  = "dashboard-lan-key.pem"

	// lanCertLife is how long a certificate Tenant makes is good for (browsers
	// refuse a server certificate good for more than 825 days), and
	// lanCertRenew how long before its end a new one is made.
	lanCertLife  = 2 * 365 * 24 * time.Hour
	lanCertRenew = 30 * 24 * time.Hour
)

// lanFlag is --dashboard-lan[=ADDR]: bare, it serves the dashboard to the
// local network for this run at the configured address; with a value, at
// that address.
type lanFlag struct {
	on   bool
	addr string
}

func (f *lanFlag) String() string { return f.addr }

func (f *lanFlag) IsBoolFlag() bool { return true }

func (f *lanFlag) Set(v string) error {
	switch v {
	case "true":
		f.on = true
	case "false":
		*f = lanFlag{}
	default:
		if _, _, err := net.SplitHostPort(v); err != nil {
			return fmt.Errorf("%s is not a host:port address", fleetQuote(v))
		}
		f.on, f.addr = true, v
	}
	return nil
}

// dashLAN is the dashboard manager's LAN listener: the operator's choice, and
// the listener while it is up. Its fields are read and written under the
// manager's lock.
type dashLAN struct {
	cfgDir string
	lc     *launchConfig // nil in a process with no config.json to write
	want   bool          // serve the local network while the dashboard runs
	addr   string        // where to listen

	cancel context.CancelFunc
	done   chan struct{}
	bound  string // the listener's address while it is up, "" otherwise
	about  string // its certificate, in words
}

// newDashLAN is the LAN listener as config.json and the launch flag ask for
// it. Nothing listens until the dashboard starts.
func newDashLAN(c *commonFlags, flag lanFlag) *dashLAN {
	l := &dashLAN{cfgDir: c.cfgDir, lc: c.lc, addr: defaultDashboardLANAddr}
	if c.lc != nil {
		l.want = c.lc.Dashboard.LAN.Enabled
		if c.lc.Dashboard.LAN.Addr != "" {
			l.addr = c.lc.Dashboard.LAN.Addr
		}
	}
	if flag.on {
		l.want = true
		if flag.addr != "" {
			l.addr = flag.addr
		}
	}
	return l
}

// lanNames is this computer's name as others on the network may type it: as
// the system gives it, its first label, and that label under .local.
func lanNames() []string {
	name, err := os.Hostname()
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if err != nil || name == "" {
		return nil
	}
	short, _, _ := strings.Cut(name, ".")
	out := []string{name}
	for _, n := range []string{short, short + ".local"} {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// lanAddresses is every address of this computer's interfaces.
func lanAddresses() []net.IP {
	out := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() {
			out = append(out, n.IP)
		}
	}
	return out
}

// certFingerprint is a certificate's SHA-256 fingerprint as a browser shows
// it.
func certFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Certificate[0])
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// newLANCertificate makes a self-signed certificate for the LAN listener,
// good from now for lanCertLife, naming this computer's names and addresses
// as they are now. A browser warns about it all the same: what the operator
// checks is its fingerprint.
func newLANCertificate(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	names := lanNames()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Tenant dashboard", Organization: []string{"Tenant"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(lanCertLife),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              append(names, "localhost"),
		IPAddresses:           lanAddresses(),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// lanCertificate is the certificate the LAN listener answers with, and what
// to tell the operator about it: the one named by dashboard.tls_cert and
// tls_key when both are set, otherwise one Tenant makes and keeps, owner-only,
// in the config dir. A kept one is used until it nears its end or no longer
// loads; then a new one is made, with a new fingerprint.
func lanCertificate(cfgDir string, lc *launchConfig) (cert tls.Certificate, about string, err error) {
	if lc != nil && lc.Dashboard.TLSCert != "" && lc.Dashboard.TLSKey != "" {
		if cert, err = tls.LoadX509KeyPair(lc.Dashboard.TLSCert, lc.Dashboard.TLSKey); err != nil {
			return cert, "", fmt.Errorf("dashboard.tls_cert and tls_key: %w", err)
		}
		return cert, "your own (dashboard.tls_cert), SHA-256 fingerprint " + certFingerprint(cert), nil
	}
	certPath, keyPath := filepath.Join(cfgDir, dashboardLANCertFile), filepath.Join(cfgDir, dashboardLANKeyFile)
	if cert, err = tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, perr := x509.ParseCertificate(cert.Certificate[0]); perr == nil && time.Now().Before(leaf.NotAfter.Add(-lanCertRenew)) {
			return cert, "self-signed, SHA-256 fingerprint " + certFingerprint(cert), nil
		}
	}
	certPEM, keyPEM, err := newLANCertificate(time.Now())
	if err != nil {
		return cert, "", fmt.Errorf("making the certificate: %w", err)
	}
	// The key first: a certificate without its key is the one case that
	// cannot be loaded, and the next start then makes both again.
	if err := secrets.WriteFile(keyPath, keyPEM); err != nil {
		return cert, "", fmt.Errorf("saving the certificate's key: %w", err)
	}
	if err := secrets.WriteFile(certPath, certPEM); err != nil {
		return cert, "", fmt.Errorf("saving the certificate: %w", err)
	}
	if cert, err = tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return cert, "", err
	}
	return cert, "self-signed, made now, SHA-256 fingerprint " + certFingerprint(cert), nil
}

// lanNote is the LAN listener as config.json asks for it, for doctor: "" while
// it is off. It reads the certificate's fingerprint and makes nothing.
func lanNote(cfgDir string, lc *launchConfig) string {
	if lc == nil || !lc.Dashboard.LAN.Enabled {
		return ""
	}
	certPath, keyPath := filepath.Join(cfgDir, dashboardLANCertFile), filepath.Join(cfgDir, dashboardLANKeyFile)
	if lc.Dashboard.TLSCert != "" && lc.Dashboard.TLSKey != "" {
		certPath, keyPath = lc.Dashboard.TLSCert, lc.Dashboard.TLSKey
	}
	fingerprint := "not made yet: it is made when the listener first starts"
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		fingerprint = "SHA-256 fingerprint " + certFingerprint(cert)
	}
	return "; also served to the local network on " + cmp.Or(lc.Dashboard.LAN.Addr, defaultDashboardLANAddr) + " over HTTPS (certificate " + fingerprint + ")"
}

// lanURLs is where a browser on the network finds a listener bound to addr:
// the address itself, or, for one bound to every interface, each private
// IPv4 address this computer has.
func lanURLs(addr string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	if ip, perr := netip.ParseAddr(host); perr != nil || !ip.IsUnspecified() {
		return []string{"https://" + net.JoinHostPort(host, port)}
	}
	var out []string
	for _, ip := range lanAddresses() {
		if v4 := ip.To4(); v4 != nil && v4.IsPrivate() {
			out = append(out, "https://"+net.JoinHostPort(v4.String(), port))
		}
	}
	if len(out) == 0 {
		out = []string{"https://<this computer's address>:" + port}
	}
	return out
}

// lanSaidLocked is the LAN listener in words. Caller holds m.mu.
func (m *dashboardManager) lanSaidLocked() string {
	l := m.lan
	if l == nil || l.bound == "" {
		return ""
	}
	return "dashboard: also serving the local network at " + strings.Join(lanURLs(l.bound), ", ") + " (listening on " + l.bound + ")\n" +
		"  certificate: " + l.about + "\n" +
		"  A browser warns about this certificate the first time: compare the fingerprint it shows with this one before you go on. " +
		"Sign in with the same key as on this computer. Only this computer and private network addresses may connect."
}

// startLANLocked brings the LAN listener up beside a running dashboard.
// Caller holds m.mu.
func (m *dashboardManager) startLANLocked() error {
	l := m.lan
	if l == nil || !m.running || m.srv == nil {
		return errors.New("the dashboard is not running: /dashboard on first")
	}
	if l.bound != "" {
		return nil
	}
	lan := dashboard.LAN{Names: lanNames()}
	if l.lc != nil {
		for _, n := range l.lc.Dashboard.LAN.Allow {
			p, err := netip.ParsePrefix(strings.TrimSpace(n))
			if err != nil {
				return fmt.Errorf("dashboard.lan.allow: %s is not a network like 100.64.0.0/10", fleetQuote(n))
			}
			lan.Allow = append(lan.Allow, p.Masked())
		}
	}
	var err error
	if lan.Cert, l.about, err = lanCertificate(l.cfgDir, l.lc); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		return err
	}
	lctx, cancel := context.WithCancel(m.base)
	done := make(chan struct{})
	srv := m.srv
	go func() {
		err := srv.ServeLAN(lctx, ln, lan)
		close(done)
		if err == nil {
			return
		}
		m.mu.Lock()
		current := l.done == done
		if current {
			l.cancel, l.done, l.bound = nil, nil, ""
		}
		m.mu.Unlock()
		if current && m.notify != nil {
			m.notify("dashboard: no longer served to the local network: " + err.Error())
		}
	}()
	l.cancel, l.done, l.bound = cancel, done, ln.Addr().String()
	return nil
}

// stopLANLocked takes the LAN listener down. Caller holds m.mu.
func (m *dashboardManager) stopLANLocked() {
	l := m.lan
	if l == nil || l.bound == "" {
		return
	}
	cancel, done := l.cancel, l.done
	l.cancel, l.done, l.bound = nil, nil, ""
	cancel()
	// The serving goroutine takes m.mu only on an error, which a cancel is not.
	<-done
}

// lanRecord files the Security record of the LAN listener turned on or off.
func lanRecord(state, address, surface string) {
	_ = catalog.DashboardLAN.Audit(context.Background(), "state", state, "address", address, "actor", actorOperator, "surface", surface)
}

// persistLAN saves the operator's choice in config.json.
func (l *dashLAN) persist() error {
	if l.lc == nil {
		return nil
	}
	return persistLaunchConfig(l.cfgDir, l.lc, func(lc *launchConfig) {
		lc.Dashboard.LAN.Enabled, lc.Dashboard.LAN.Addr = l.want, l.addr
	})
}

// ServeLAN is /dashboard serve lan [ADDR]: it serves the running dashboard to
// the local network at addr ("" keeps the address in use, the default the
// first time), saves the choice, and says where and with which certificate.
func (m *dashboardManager) ServeLAN(addr string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lan
	if l == nil {
		return "", errors.New("serving the dashboard to the local network is not available in this session")
	}
	if addr != "" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", fmt.Errorf("%s is not a host:port address, like %s", fleetQuote(addr), defaultDashboardLANAddr)
		}
	}
	if !m.running {
		return "", errors.New("the dashboard is not running: /dashboard on first")
	}
	was := l.addr
	if addr != "" && addr != l.addr {
		m.stopLANLocked() // it moves
		l.addr = addr
	}
	fresh := l.bound == ""
	if err := m.startLANLocked(); err != nil {
		l.addr = was
		if l.want { // what it was serving before a move that failed
			_ = m.startLANLocked()
		}
		return "", err
	}
	l.want = true
	if fresh {
		lanRecord("on", l.bound, surfaceTUI)
	}
	return m.lanSaidLocked(), l.persist()
}

// StopLAN is /dashboard serve off: the LAN listener comes down, its open
// connections with it, and the choice is saved.
func (m *dashboardManager) StopLAN() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lan
	if l == nil || !l.want && l.bound == "" {
		return "dashboard: it is not served to the local network", nil
	}
	was := l.bound
	m.stopLANLocked()
	l.want = false
	lanRecord("off", was, surfaceTUI)
	return "dashboard: no longer served to the local network; this computer and the tailnet reach it as before", l.persist()
}

// LANStatus is the LAN listener in words, "" while it is off.
func (m *dashboardManager) LANStatus() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lanSaidLocked()
}
