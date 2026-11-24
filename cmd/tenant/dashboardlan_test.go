package main

// The dashboard served to the local network (TEN-544), on real listeners.
// Every listener here is on a loopback address: a first listener on another
// one puts a firewall prompt on the screen of whoever is using the computer
// the tests run on.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/logs/catalog"
)

const lanTestKey = "lan-test-key-0123456789abcdef0123456789"

var lanURL = regexp.MustCompile(`https://127\.0\.0\.1:\d+`)
var lanFingerprint = regexp.MustCompile(`SHA-256 fingerprint ((?:[0-9A-F]{2}:){31}[0-9A-F]{2})`)

// lanLab is a dashboard manager on a loopback port with the LAN listener
// offered, and what it told the operator.
type lanLab struct {
	t    *testing.T
	m    *dashboardManager
	c    *commonFlags
	mu   sync.Mutex
	told []string
}

func newLANLab(t *testing.T, flagged lanFlag) *lanLab {
	t.Helper()
	cfg := t.TempDir()
	lc := &launchConfig{}
	lc.Dashboard.LAN.Addr = "127.0.0.1:0"
	l := &lanLab{t: t, c: &commonFlags{cfgDir: cfg, dataDir: t.TempDir(), agent: "main", lc: lc}}
	l.m = &dashboardManager{
		base:   context.Background(),
		cfg:    dashboard.Config{Addr: "127.0.0.1:0", Auth: lanTestKey},
		notify: func(s string) { l.mu.Lock(); l.told = append(l.told, s); l.mu.Unlock() },
		lan:    newDashLAN(l.c, flagged),
	}
	t.Cleanup(func() { _ = l.m.Shutdown() })
	return l
}

func (l *lanLab) said() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.told, "\n")
}

// client is a browser that trusts exactly the certificate in the config dir.
func (l *lanLab) client() *http.Client {
	l.t.Helper()
	raw, err := os.ReadFile(filepath.Join(l.c.cfgDir, dashboardLANCertFile))
	if err != nil {
		l.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		l.t.Fatal("the kept certificate is not one")
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, DisableKeepAlives: true}}
}

func httpStatus(t *testing.T, c *http.Client, url string) (int, error) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// Off until asked for. Turned on, the running dashboard is also served over
// HTTPS on a second listener, with a certificate Tenant makes once and keeps
// owner-only; the loopback listener is untouched. Turned off, the second
// listener is gone and the first is still there. The choice is saved, each
// change is in the Security log, and the listener follows the dashboard down
// and up.
func TestDashboardLAN(t *testing.T) {
	ev := captureChannelEvents(t)
	l := newLANLab(t, lanFlag{})
	m := l.m
	if _, err := m.ServeLAN(""); err == nil || !strings.Contains(err.Error(), "/dashboard on") {
		t.Fatalf("serving the LAN with the dashboard off: %v", err)
	}
	addr, err := m.Enable()
	if err != nil {
		t.Fatal(err)
	}
	if on := m.LANStatus(); on != "" || l.said() != "" {
		t.Fatalf("a dashboard nobody asked to serve the LAN says: %q / %q", on, l.said())
	}
	if _, err := os.Stat(filepath.Join(l.c.cfgDir, dashboardLANKeyFile)); err == nil {
		t.Fatal("a certificate was made before the LAN was asked for")
	}
	if _, err := m.ServeLAN("nine"); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("an address that is not one: %v", err)
	}

	said, err := m.ServeLAN("")
	if err != nil {
		t.Fatal(err)
	}
	url, fp := lanURL.FindString(said), lanFingerprint.FindStringSubmatch(said)
	if url == "" || fp == nil || !strings.Contains(said, "self-signed") || !strings.Contains(said, "Only this computer and private network addresses") {
		t.Fatalf("/dashboard serve lan said:\n%s", said)
	}
	// HTTPS with the kept certificate, and nothing else on that port.
	c := l.client()
	if code, err := httpStatus(t, c, url+"/healthz"); err != nil || code != http.StatusOK {
		t.Fatalf("the LAN listener: %d %v", code, err)
	}
	if code, err := httpStatus(t, http.DefaultClient, "http"+strings.TrimPrefix(url, "https")+"/healthz"); err == nil && code == http.StatusOK {
		t.Fatal("the LAN listener answered plain HTTP")
	}
	if code, err := httpStatus(t, c, url+"/api/tools"); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("the LAN listener without a key: %d %v", code, err)
	}
	// The fingerprint it told is the certificate it serves.
	conn, err := tls.Dial("tcp", strings.TrimPrefix(url, "https://"), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	served := certFingerprint(tls.Certificate{Certificate: [][]byte{conn.ConnectionState().PeerCertificates[0].Raw}})
	_ = conn.Close()
	if served != fp[1] {
		t.Fatalf("it serves %s and told the operator %s", served, fp[1])
	}
	// The loopback listener is as it was.
	if code, err := httpStatus(t, http.DefaultClient, "http://"+addr+"/healthz"); err != nil || code != http.StatusOK {
		t.Fatalf("the dashboard's own listener while the LAN is served: %d %v", code, err)
	}
	// The key is the owner's alone, and the certificate is kept: asked again,
	// and after a stop and a start, the fingerprint is the same.
	if fi, err := os.Stat(filepath.Join(l.c.cfgDir, dashboardLANKeyFile)); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("the certificate's key: %v %v", fi, err)
	}
	if again, err := m.ServeLAN(""); err != nil || lanFingerprint.FindStringSubmatch(again)[1] != fp[1] || lanURL.FindString(again) != url {
		t.Fatalf("asked again:\n%s\n%v", again, err)
	}
	if !l.c.lc.Dashboard.LAN.Enabled {
		t.Fatal("the choice was not saved")
	}
	if saved, err := loadLaunchConfig(l.c.cfgDir); err != nil || !saved.Dashboard.LAN.Enabled || saved.Dashboard.LAN.Addr != "127.0.0.1:0" {
		t.Fatalf("config.json: %+v %v", saved.Dashboard.LAN, err)
	}

	// The dashboard off takes the LAN listener with it; on again, it is back,
	// because the operator's choice stands.
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := httpStatus(t, c, url+"/healthz"); err == nil || m.LANStatus() != "" {
		t.Fatalf("the LAN listener outlived the dashboard: %v, %q", err, m.LANStatus())
	}
	if _, err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	back := m.LANStatus()
	if lanFingerprint.FindStringSubmatch(back) == nil || lanFingerprint.FindStringSubmatch(back)[1] != fp[1] {
		t.Fatalf("after the dashboard came back:\n%s", back)
	}
	if code, err := httpStatus(t, c, lanURL.FindString(back)+"/healthz"); err != nil || code != http.StatusOK {
		t.Fatalf("the LAN listener after the dashboard came back: %d %v", code, err)
	}
	eventually(t, "the operator is told the LAN is served again", func() bool { return strings.Contains(l.said(), "also serving the local network") })

	// Off: the listener is gone, the choice is saved, the dashboard stays.
	off, err := m.StopLAN()
	if err != nil || !strings.Contains(off, "no longer served to the local network") {
		t.Fatalf("/dashboard serve off: %q %v", off, err)
	}
	if _, err := httpStatus(t, c, lanURL.FindString(back)+"/healthz"); err == nil || m.LANStatus() != "" || l.c.lc.Dashboard.LAN.Enabled {
		t.Fatalf("after serve off: %v, %q, saved on=%v", err, m.LANStatus(), l.c.lc.Dashboard.LAN.Enabled)
	}
	if running, a := m.Status(); !running {
		t.Fatalf("serve off stopped the dashboard (%s)", a)
	}
	if again, err := m.StopLAN(); err != nil || !strings.Contains(again, "not served") {
		t.Fatalf("serve off twice: %q %v", again, err)
	}

	var states []string
	for _, e := range ev.matching(catalog.DashboardLAN, "") {
		for _, f := range e.Data {
			if f.Key == "state" {
				states = append(states, f.Text()+"@"+e.Surface)
			}
		}
	}
	if got := strings.Join(states, " "); got != "on@tui on@cli off@tui" {
		t.Fatalf("the Security log of the LAN listener: %q", got)
	}
}

// config.json and the launch flag each turn it on at start; the flag's
// address wins, and neither is the operator's saved choice.
func TestDashboardLANAtLaunch(t *testing.T) {
	var f lanFlag
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&f, "dashboard-lan", "")
	for args, want := range map[string]lanFlag{
		"":                               {},
		"--dashboard-lan":                {on: true},
		"--dashboard-lan=127.0.0.1:9443": {on: true, addr: "127.0.0.1:9443"},
		"--dashboard-lan=false":          {},
	} {
		f = lanFlag{}
		if err := fs.Parse(strings.Fields(args)); err != nil || f != want {
			t.Errorf("%q: %+v %v, want %+v", args, f, err, want)
		}
	}
	if err := fs.Parse([]string{"--dashboard-lan=nine"}); err == nil {
		t.Error("--dashboard-lan=nine was taken")
	}

	// Nothing asks: off, at the default address.
	if l := newDashLAN(&commonFlags{lc: &launchConfig{}}, lanFlag{}); l.want || l.addr != defaultDashboardLANAddr {
		t.Fatalf("with nothing asking: %+v", l)
	}
	// The flag: the dashboard comes up serving the LAN, and config.json is
	// not written.
	l := newLANLab(t, lanFlag{on: true, addr: "127.0.0.1:0"})
	if _, err := l.m.Enable(); err != nil {
		t.Fatal(err)
	}
	on := l.m.LANStatus()
	if lanURL.FindString(on) == "" {
		t.Fatalf("started with --dashboard-lan:\n%s", on)
	}
	if code, err := httpStatus(t, l.client(), lanURL.FindString(on)+"/healthz"); err != nil || code != http.StatusOK {
		t.Fatalf("the LAN listener at launch: %d %v", code, err)
	}
	if l.c.lc.Dashboard.LAN.Enabled {
		t.Fatal("the flag was saved as the operator's choice")
	}
	eventually(t, "the operator is told at launch", func() bool { return strings.Contains(l.said(), "also serving the local network") })

	// config.json: the same, and an address that cannot be bound is told and
	// leaves the dashboard up.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	l2 := newLANLab(t, lanFlag{})
	l2.c.lc.Dashboard.LAN.Enabled, l2.c.lc.Dashboard.LAN.Addr = true, taken.Addr().String()
	l2.m.lan = newDashLAN(l2.c, lanFlag{})
	addr, err := l2.m.Enable()
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the operator is told the LAN is not served", func() bool { return strings.Contains(l2.said(), "not served to the local network") })
	if code, err := httpStatus(t, http.DefaultClient, "http://"+addr+"/healthz"); err != nil || code != http.StatusOK || l2.m.LANStatus() != "" {
		t.Fatalf("the dashboard beside a LAN address that is taken: %d %v, %q", code, err, l2.m.LANStatus())
	}
}

// The operator's own certificate (dashboard.tls_cert and tls_key) is served
// in place of one Tenant makes, and a network in dashboard.lan.allow that is
// not one refuses to start.
func TestDashboardLANOwnCertificateAndAllow(t *testing.T) {
	l := newLANLab(t, lanFlag{})
	certPEM, keyPEM, err := newLANCertificate(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.m.Enable(); err != nil {
		t.Fatal(err)
	}
	l.c.lc.Dashboard.LAN.Allow = []string{"100.64.0.0/10", "the office"}
	if _, err := l.m.ServeLAN(""); err == nil || !strings.Contains(err.Error(), "dashboard.lan.allow") {
		t.Fatalf("a network that is not one: %v", err)
	}
	l.c.lc.Dashboard.LAN.Allow = []string{"100.64.0.0/10"}
	l.c.lc.Dashboard.TLSCert, l.c.lc.Dashboard.TLSKey = certPath, keyPath
	said, err := l.m.ServeLAN("")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	want := certFingerprint(tls.Certificate{Certificate: [][]byte{block.Bytes}})
	if fp := lanFingerprint.FindStringSubmatch(said); fp == nil || fp[1] != want || !strings.Contains(said, "your own") {
		t.Fatalf("with the operator's certificate:\n%s\nwant fingerprint %s", said, want)
	}
	if _, err := os.Stat(filepath.Join(l.c.cfgDir, dashboardLANKeyFile)); err == nil {
		t.Fatal("Tenant made a certificate beside the operator's own")
	}
}
