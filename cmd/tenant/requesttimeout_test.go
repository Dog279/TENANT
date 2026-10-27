package main

import (
	"strings"
	"testing"
	"time"
)

// providers.<name>.request_timeout reaches every generation profile the
// provider builds, and a value that isn't a positive duration is refused.
func TestRequestTimeout_ProviderToProfiles(t *testing.T) {
	for _, p := range vllmGenProfiles(genProfileOpts{endpoint: "https://x", model: "m", requestTimeout: 90 * time.Second}) {
		if p.RequestTimeout != 90*time.Second {
			t.Errorf("%s: request timeout %s, want 90s", p.ID, p.RequestTimeout)
		}
	}
	for _, c := range []struct {
		in   string
		want time.Duration
		err  string
	}{
		{"", 0, ""}, {"90s", 90 * time.Second, ""}, {" 2m ", 2 * time.Minute, ""},
		{"soon", 0, "not a duration"}, {"-1s", 0, "not a duration"}, {"0", 0, "not a duration"},
	} {
		got, err := requestTimeoutOf(&providerConfig{RequestTimeout: c.in})
		switch {
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%q: got %s, %v; want %s", c.in, got, err, c.want)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%q: err %v, want %q", c.in, err, c.err)
		}
	}
}
