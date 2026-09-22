//go:build darwin

package sandbox

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// Escape case 9 (TEN-347): a command must not reach Tenant's own loopback
// listeners (the dashboard's /api/approvals) under a no-network profile. A local
// TCP listener stands in for the dashboard; the legit twin (full network) proves
// the probe itself works, so a pass under NetNone is containment, not a broken
// probe.
func TestSeatbelt_NetNoneBlocksLoopbackListeners(t *testing.T) {
	b := seatbeltOrSkip(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	probe := Command{Shell: "nc -z -w 2 127.0.0.1 " + port}
	base := Policy{WorkDir: t.TempDir(), Limits: Limits{Wall: 10 * time.Second}}

	full := base
	full.Network = NetFull
	if res, _ := b.Run(context.Background(), probe, full); res.ExitCode != 0 {
		t.Fatalf("legit twin: with full network the loopback probe must connect: %+v", res)
	}

	none := base
	none.Network = NetNone
	res, err := b.Run(context.Background(), probe, none)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Contained() {
		t.Fatalf("NetNone must block connecting to a loopback listener (the dashboard API): %+v", res)
	}
}

// Nothing enforces a host allowlist yet, so NetAllowlist must fail closed on
// Seatbelt: no network at all rather than all of it.
func TestSeatbelt_NetAllowlistFailsClosed(t *testing.T) {
	b := seatbeltOrSkip(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	pol := Policy{WorkDir: t.TempDir(), Network: NetAllowlist, Allow: []string{"127.0.0.1"},
		Limits: Limits{Wall: 10 * time.Second}}
	res, err := b.Run(context.Background(), Command{Shell: "nc -z -w 2 127.0.0.1 " + port}, pol)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Contained() {
		t.Fatalf("NetAllowlist must fail closed until an allowlist is enforced: %+v", res)
	}
}
