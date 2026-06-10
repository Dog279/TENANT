package web

import "testing"

// A resolver func wins over the static field and is re-read on EVERY call, so a
// rotated key is picked up live (the whole point of TEN-147).
func TestConfigLazyKeyResolvers(t *testing.T) {
	calls := 0
	cfg := Config{TavilyKey: "static", TavilyKeyFunc: func() string { calls++; return "live" }}
	if got := cfg.tavilyKey(); got != "live" {
		t.Errorf("resolver should win over static, got %q", got)
	}
	_ = cfg.tavilyKey()
	if calls != 2 {
		t.Errorf("resolver must be re-read each call, got %d calls", calls)
	}
	// nil func → static fallback (back-compat).
	if (Config{BraveKey: "b"}).braveKey() != "b" {
		t.Error("nil resolver should fall back to the static field")
	}
	if (Config{JinaKey: "j"}).jinaKey() != "j" {
		t.Error("nil jina resolver should fall back to static")
	}
}

// NewSearcher must reflect the CURRENT resolver value each time it's called, so
// adding/removing a key at runtime changes the selected backend live.
func TestNewSearcherLive(t *testing.T) {
	key := ""
	cfg := Config{TavilyKeyFunc: func() string { return key }}

	if _, ok := NewSearcher(cfg).(*ddgSearcher); !ok {
		t.Error("empty key → DuckDuckGo")
	}
	key = "tav-key" // rotate in
	if _, ok := NewSearcher(cfg).(*tavilySearcher); !ok {
		t.Error("after key added, NewSearcher should pick Tavily (live re-read)")
	}
	key = "" // rotate out
	if _, ok := NewSearcher(cfg).(*ddgSearcher); !ok {
		t.Error("after key removed, NewSearcher should fall back to DuckDuckGo")
	}
}
