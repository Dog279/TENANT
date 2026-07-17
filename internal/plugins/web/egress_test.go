package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// staticResolver is a deterministic DNS stub for the egress-guard tests: it maps
// hostnames to fixed addresses and refuses any lookup it was not primed for, so a
// test can prove the guard checks resolved IPs without real DNS. (Originally
// defined in reader_test.go on the TEN-307 branch; relocated here during the
// audit integration merge since TEN-319's reader_test.go replaced that file.)
type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, errors.New("unexpected DNS lookup for " + host)
	}
	return addrs, nil
}

func TestValidateURLBlocksDirectTargets(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"IPv4 loopback", "http://127.0.0.1/"},
		{"IPv4 private 10", "http://10.1.2.3/"},
		{"IPv4 private 172", "http://172.16.0.1/"},
		{"IPv4 private 192", "http://192.168.1.1/"},
		{"IPv4 link local metadata", "http://169.254.169.254/latest/meta-data/"},
		{"IPv4 multicast", "http://224.0.0.1/"},
		{"IPv4 unspecified", "http://0.0.0.0/"},
		{"metadata Alibaba", "http://100.100.100.200/"},
		{"metadata Azure", "http://168.63.129.16/"},
		{"decimal IPv4", "http://2130706433/"},
		{"hex IPv4", "http://0x7f000001/"},
		{"octal IPv4", "http://017700000001/"},
		{"short IPv4", "http://127.1/"},
		{"mixed IPv4", "http://0x7f.0.0.1/"},
		{"IPv6 loopback", "http://[::1]/"},
		{"IPv6 link local", "http://[fe80::1]/"},
		{"IPv6 private", "http://[fd00::1]/"},
		{"IPv6 multicast", "http://[ff02::1]/"},
		{"IPv6 unspecified", "http://[::]/"},
		{"IPv4 mapped IPv6", "http://[::ffff:127.0.0.1]/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := validateURL(tt.url); err == nil {
				t.Fatalf("validateURL(%q) allowed a forbidden destination", tt.url)
			}
		})
	}
	for _, raw := range []string{"https://93.184.216.34/", "https://[2606:4700:4700::1111]/", "https://dead.beef/"} {
		if _, err := validateURL(raw); err != nil {
			t.Errorf("public destination %q rejected: %v", raw, err)
		}
	}
}

func TestEgressGuardChecksEveryDNSResult(t *testing.T) {
	resolver := staticResolver{
		"private.test": {netip.MustParseAddr("10.0.0.7")},
		"mixed.test": {
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("127.0.0.1"),
		},
		"public.test": {netip.MustParseAddr("93.184.216.34")},
	}
	guard := newEgressGuard(Config{resolver: resolver})
	for _, host := range []string{"private.test", "mixed.test"} {
		if _, err := guard.ValidateURL(context.Background(), "https://"+host+"/"); err == nil {
			t.Errorf("%s should be blocked", host)
		}
	}
	if _, err := guard.ValidateURL(context.Background(), "https://public.test/"); err != nil {
		t.Fatalf("public DNS result rejected: %v", err)
	}
}

func TestNavigateRejectsPrivateDNSBeforeBrowser(t *testing.T) {
	fake := &fakeBrowser{}
	d := newDispatcherWithBrowser(fake, Policy{}, ".")
	d.guard = newEgressGuard(Config{resolver: staticResolver{
		"private.test": {netip.MustParseAddr("192.168.20.5")},
	}})
	d.cfg.ReaderEnabled = false // no reader fallback: the private-target block must surface directly
	out, isErr, err := d.Dispatch(context.Background(), call("web_navigate", map[string]any{"url": "http://private.test/"}))
	if err != nil || !isErr || !strings.Contains(out, "private or non-public") {
		t.Fatalf("private navigation result: out=%q isErr=%v err=%v", out, isErr, err)
	}
	if fake.navURL != "" {
		t.Fatalf("browser received forbidden URL %q", fake.navURL)
	}
}

func TestSearchBackendUsesGuardedHTTPClient(t *testing.T) {
	searcher := NewSearcher(Config{
		BraveKey: "test",
		resolver: staticResolver{
			"api.search.brave.com": {netip.MustParseAddr("127.0.0.1")},
		},
	})
	_, err := searcher.Search(context.Background(), "ssrf", 1)
	if err == nil || !strings.Contains(err.Error(), "private or non-public") {
		t.Fatalf("search request should be blocked before dialing, got %v", err)
	}
}

func TestPrivateNetworkAllowlistRequiresExactNamedHost(t *testing.T) {
	resolver := staticResolver{
		"service.internal":       {netip.MustParseAddr("10.2.3.4")},
		"other.service.internal": {netip.MustParseAddr("10.2.3.5")},
	}
	guard := newEgressGuard(Config{
		PrivateNetwork: []string{"SERVICE.INTERNAL."},
		resolver:       resolver,
	})
	if _, err := guard.ValidateURL(context.Background(), "http://service.internal/"); err != nil {
		t.Fatalf("explicitly named private endpoint rejected: %v", err)
	}
	if _, err := guard.ValidateURL(context.Background(), "http://other.service.internal/"); err == nil {
		t.Fatal("allowlist must use exact host matching")
	}
	guard = newEgressGuard(Config{PrivateNetwork: []string{"127.0.0.1"}})
	if _, err := guard.ValidateURL(context.Background(), "http://127.0.0.1/"); err == nil {
		t.Fatal("numeric targets must not be allowlistable")
	}
}

func TestDialContextPinsResolvedIP(t *testing.T) {
	var dialed string
	guard := newEgressGuard(Config{
		resolver: staticResolver{"public.test": {netip.MustParseAddr("93.184.216.34")}},
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = address
			client, peer := net.Pipe()
			_ = peer.Close()
			return client, nil
		},
	})
	conn, err := guard.DialContext(context.Background(), "tcp", "public.test:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if dialed != "93.184.216.34:443" {
		t.Fatalf("system dialer received %q, want pinned IP literal", dialed)
	}
}

type sequenceResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   int
}

func (r *sequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls >= len(r.answers) {
		return nil, errors.New("unexpected extra DNS lookup")
	}
	answer := r.answers[r.calls]
	r.calls++
	return answer, nil
}

func TestGuardedHTTPClientBlocksRebindingBetweenCheckAndDial(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("93.184.216.34")},
		{netip.MustParseAddr("127.0.0.1")},
	}}
	client := guardedHTTPClient(newEgressGuard(Config{resolver: resolver}), 0)
	_, err := client.Get("http://rebind.test/")
	if err == nil || !strings.Contains(err.Error(), "private or non-public") {
		t.Fatalf("rebind to loopback should be blocked, got %v", err)
	}
	if resolver.calls != 2 {
		t.Fatalf("DNS calls = %d, want validation plus pinned dial", resolver.calls)
	}
}

func TestGuardedHTTPClientBlocksPublicToPrivateRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://private.test/secret", http.StatusFound)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	resolver := staticResolver{
		"public.test":  {netip.MustParseAddr("127.0.0.1")},
		"private.test": {netip.MustParseAddr("10.0.0.9")},
	}
	guard := newEgressGuard(Config{
		PrivateNetwork: []string{"public.test"},
		resolver:       resolver,
	})
	client := guardedHTTPClient(guard, 0)
	_, err := client.Get("http://public.test:" + u.Port() + "/")
	if err == nil || !strings.Contains(err.Error(), "private.test") {
		t.Fatalf("public-to-private redirect should be blocked, got %v", err)
	}
}

func TestChromeEgressProxyUsesGuardedDialer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://blocked.test:"+r.URL.Query().Get("port")+"/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	guard := newEgressGuard(Config{
		PrivateNetwork: []string{"allowed.test"},
		resolver: staticResolver{
			"allowed.test": {netip.MustParseAddr("127.0.0.1")},
			"blocked.test": {netip.MustParseAddr("127.0.0.1")},
		},
	})
	proxy, err := startEgressProxy(guard)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxyURL, _ := url.Parse("http://" + proxy.Address())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get("http://allowed.test:" + u.Port() + "/")
	if err != nil {
		t.Fatalf("allowlisted request through Chrome proxy failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted status = %d", resp.StatusCode)
	}
	resp, err = client.Get("http://blocked.test:" + u.Port() + "/")
	if err != nil {
		t.Fatalf("proxy should return an HTTP block response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("blocked status = %d, want 502", resp.StatusCode)
	}
	resp, err = client.Get("http://allowed.test:" + u.Port() + "/redirect?port=" + u.Port())
	if err != nil {
		t.Fatalf("redirect through proxy returned an unexpected client error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("redirect-to-private status = %d, want 502", resp.StatusCode)
	}
}
