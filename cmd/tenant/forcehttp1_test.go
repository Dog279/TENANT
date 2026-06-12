package main

import "testing"

// TEN-218: the Z.ai kinds force HTTP/1.1 (their LB recycles long-lived h2
// connections with a mid-stream GOAWAY); other hosted kinds keep HTTP/2.
func TestZaiKindsForceHTTP1(t *testing.T) {
	for _, id := range []string{"zai", "zai-coding", "zai-coding-cn", "zai-metered"} {
		if !providerKinds[id].ForceHTTP1 {
			t.Errorf("provider kind %q should force HTTP/1.1 (GOAWAY mitigation)", id)
		}
	}
	if providerKinds["openai"].ForceHTTP1 {
		t.Error("openai should NOT force HTTP/1.1")
	}
	if providerKinds["vllm"].ForceHTTP1 {
		t.Error("self-hosted vllm should NOT force HTTP/1.1")
	}
}

// TEN-218: forceHTTP1 is plumbed from genProfileOpts onto every gen profile,
// and is off by default.
func TestVllmGenProfilesPlumbsForceHTTP1(t *testing.T) {
	on := vllmGenProfiles(genProfileOpts{endpoint: "https://x", model: "m", forceHTTP1: true})
	if len(on) == 0 {
		t.Fatal("no profiles built")
	}
	for _, p := range on {
		if !p.ForceHTTP11 {
			t.Errorf("profile %s: ForceHTTP11 not plumbed from genProfileOpts.forceHTTP1", p.ID)
		}
	}
	for _, p := range vllmGenProfiles(genProfileOpts{endpoint: "https://x", model: "m"}) {
		if p.ForceHTTP11 {
			t.Errorf("profile %s: ForceHTTP11 should default false", p.ID)
		}
	}
}
