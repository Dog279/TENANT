package logs

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestHandler_FixedSourceAndFields(t *testing.T) {
	h := NewMemory(10)
	l := h.Logger("discord")
	l.Info("relay connected", "guild", "g1", "n", 3, "err", errors.New("nope"))
	all, _ := h.Snapshot()
	if len(all) != 1 || all[0].Source != "discord" || all[0].Msg != "relay connected" {
		t.Fatalf("entry = %+v", all)
	}
	if all[0].Field("guild") != "g1" || all[0].Field("n") != "3" || all[0].Field("err") != "nope" {
		t.Errorf("fields = %+v", all[0].Fields)
	}
}

func TestHandler_RoutesByAttrThenPrefixThenDefault(t *testing.T) {
	h := NewMemory(10)
	l := slog.New(h.Handler(nil))
	l.Warn("mcp: token refresh failed", "err", "x")                       // prefix table → mcp
	l.Info("vllm: skipping malformed sse frame")                          // → model
	l.Info("settings save failed")                                        // no prefix → system
	l.With("source", "cron").Info("mcp: pinned")                          // attr wins over prefix, and is consumed
	l.Info("relay attachment download failed", "source", "Discord Relay") // record attr, normalized
	l.Info("unknown-tag: something")                                      // unknown tag → system (no folder minted)
	all, _ := h.Snapshot()
	want := []string{"mcp", "model", "system", "cron", "discord-relay", "system"}
	for i, w := range want {
		if all[i].Source != w {
			t.Errorf("entry %d (%q) source = %q, want %q", i, all[i].Msg, all[i].Source, w)
		}
	}
	if all[3].Field("source") != "" || all[4].Field("source") != "" {
		t.Error("the source attr must not be filed as a field")
	}
	if all[0].Field("err") != "x" {
		t.Error("other attrs are kept")
	}
}

func TestHandler_TeeKeepsItsOwnLevel(t *testing.T) {
	h := NewMemory(10)
	var buf bytes.Buffer
	tee := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	l := slog.New(h.Handler(tee)).With("k", "v")
	l.Info("quiet info")
	l.Warn("loud warn")
	if all, _ := h.Snapshot(); len(all) != 2 || all[0].Field("k") != "v" {
		t.Errorf("hub should hold both with the With attr: %+v", all)
	}
	out := buf.String()
	if strings.Contains(out, "quiet info") || !strings.Contains(out, "loud warn") || !strings.Contains(out, "k=v") {
		t.Errorf("tee output = %q", out)
	}
}

func TestHandler_GroupsPrefixKeys(t *testing.T) {
	h := NewMemory(10)
	l := slog.New(h.Handler(nil)).WithGroup("req").With("id", 7)
	l.Info("m", "path", "/x", slog.Group("peer", "ip", "10.0.0.1"))
	all, _ := h.Snapshot()
	if all[0].Field("req.id") != "7" || all[0].Field("req.path") != "/x" || all[0].Field("req.peer.ip") != "10.0.0.1" {
		t.Errorf("fields = %+v", all[0].Fields)
	}
}

func TestHandler_DebugFollowsHubFloor(t *testing.T) {
	h := NewMemory(10)
	l := slog.New(h.Handler(nil))
	if l.Enabled(nil, slog.LevelDebug) {
		t.Error("DEBUG disabled at the default INFO floor")
	}
	l.Debug("dropped")
	hd, _ := Open("", Options{RingCap: 10, MinLevel: slog.LevelDebug})
	ld := slog.New(hd.Handler(nil))
	if !ld.Enabled(nil, slog.LevelDebug) {
		t.Error("DEBUG enabled when the hub floor is DEBUG")
	}
	ld.Debug("kept")
	if all, _ := h.Snapshot(); len(all) != 0 {
		t.Error("debug should be dropped")
	}
	if all, _ := hd.Snapshot(); len(all) != 1 {
		t.Error("debug should be kept")
	}
}

func TestFor_ResolvesHubLazily(t *testing.T) {
	Install(nil)
	l := For("myfeature") // built before any hub exists — must not panic
	l.Info("before install (stderr fallback drops INFO)")
	h := NewMemory(10)
	Install(h)
	defer Install(nil)
	l.Info("after install", "k", "v")
	all, _ := h.Snapshot()
	if len(all) != 1 || all[0].Source != "myfeature" || all[0].Field("k") != "v" {
		t.Fatalf("For logger should file into the installed hub: %+v", all)
	}
	if Default() != h {
		t.Error("Default() should be the installed hub")
	}
}
