package peering

import (
	"context"
	"strings"
	"testing"
	"time"
)

// security renders the Security records captured, "msg | actor@surface
// k=v…" each.
func (c *captured) security() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Log != "security" {
			continue
		}
		l := e.Msg + " | " + e.Actor + "@" + e.Surface
		for _, f := range e.Data {
			if f.Key != "remote" {
				l += " " + f.Key + "=" + f.Text()
			}
		}
		out = append(out, l)
	}
	return out
}

// Pairing, an invite code, revoking, removing and a refused token are
// Security records; removing a peer that doesn't exist is not.
func TestPeeringSecurityRecords(t *testing.T) {
	srv, _, _ := pairedListener(t, nil) // mints an invite for "spoke"
	defer srv.Close()
	c := capture(t)
	s, _ := LoadStore(t.TempDir())
	if err := s.Put(&Peer{Name: "bob", InstanceID: "i-bob", Dial: true, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite("me", "i-me", "https://me", "", time.Hour, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revoke("bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("bob"); err != nil {
		t.Fatal(err)
	}
	_ = s.Remove("nobody")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if bad, err := dialClient(t, ctx, srv.URL, "nope"); err == nil {
		bad.Close()
		t.Fatal("a bad token connected")
	}

	want := []string{
		"Paired with peer bob | operator@peering peer=bob instance=i-bob direction=outbound",
		"Paired with peer carol | operator@peering peer=carol instance= direction=inbound",
		"Unpaired peer bob (revoke) | operator@peering peer=bob how=revoke",
		"Unpaired peer bob (remove) | operator@peering peer=bob how=remove",
	}
	got := c.security()
	if len(got) < len(want)+1 || strings.Join(got[:len(want)], "\n") != strings.Join(want, "\n") ||
		!strings.Contains(got[len(want)], "refused: token") || !strings.HasSuffix(got[len(want)], "@peering reason=token method=POST route=/") {
		t.Errorf("security records:\n%s\nwant:\n%s\nthen a refused token", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
