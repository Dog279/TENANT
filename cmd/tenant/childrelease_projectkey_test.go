package main

// childrelease_projectkey_test.go (TEN-542): a child on the default rule
// whose binary carries the project key takes a release whose project
// signature verifies, with no tool on the machine; a project signature that
// is there and wrong refuses at once and never falls through to cosign; with
// none sent it is cosign's word as before. A child with no key is unchanged.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"tenant/internal/fleet"
)

func TestChildProjectKeyRefusals(t *testing.T) {
	ctx := context.Background()
	genuine := bytes.Repeat([]byte("genuine "), 64)
	asset, sums, bundle := release("3.0.0", genuine, testReleaseRepo)
	priv1, pub1 := projectKey(1)
	priv2, pub2 := projectKey(2)
	priv3, _ := projectKey(3)
	good := projectSign(priv1, sums)
	tamperedSums := append(bytes.Clone(sums), []byte(strings.Repeat("1", 64)+"  tenant_3.0.0_plan9_arm.tar.gz\n")...)
	withSig := func(sums, bundle, sig []byte) fleet.Update {
		p := opening("3.0.0", genuine, asset, sums, bundle)
		p.Signature = sig
		return p
	}
	noCosign := func(context.Context, ...string) error {
		return errors.New("cosign is not installed on this machine, and the release's signature is verified with it")
	}

	for name, tc := range map[string]struct {
		keys     string
		p        fleet.Update
		noCosign bool
		want     string
	}{
		"tampered checksums":           {pub1, withSig(tamperedSums, nil, good), true, "project signature does not verify"},
		"a signature by another key":   {pub1 + "," + pub2, withSig(sums, nil, projectSign(priv3, sums)), true, "project signature does not verify"},
		"a signature for another file": {pub1, withSig(sums, nil, projectSign(priv1, []byte("another file"))), true, "project signature does not verify"},
		"a malformed signature":        {pub1, withSig(sums, nil, []byte("not a signature")), true, "malformed"},
		"an over-long signature":       {pub1, withSig(sums, nil, bytes.Repeat([]byte("A"), fleet.MaxReleaseSignature)), true, "malformed"},
		"a key that does not read":     {pub1 + ",", withSig(sums, bundle, good), false, "project key does not read"},
		"an over-long key list":        {strings.Repeat(pub1, 4), withSig(sums, bundle, good), false, "project key does not read"},
		// Present and wrong: refused even though cosign would verify the bundle.
		"a wrong signature beside a good bundle": {pub1, withSig(sums, bundle, projectSign(priv3, sums)), false, "project signature does not verify"},
		// None sent: cosign only, and it says so when cosign is not there.
		"no signature for a binary that carries a key": {pub1, withSig(sums, bundle, nil), true, "sent no project signature with the release"},
		"neither signature nor bundle":                 {pub1, withSig(sums, nil, nil), false, "neither a project signature nor a signature bundle"},
		// A good signature does not make a wrong archive name right.
		"a good signature, another system's archive": {pub1, func() fleet.Update { p := withSig(sums, nil, good); p.Asset = "tenant_3.0.0_plan9_arm.tar.gz"; return p }(), true, "not " + asset},
	} {
		t.Run(name, func(t *testing.T) {
			u, dir := releaseChild(t)
			carryKeys(t, tc.keys)
			cosignRan := 0
			check := childCosign
			if tc.noCosign {
				check = noCosign
			}
			childCosign = func(ctx context.Context, args ...string) error { cosignRan++; return check(ctx, args...) }
			ack, err := u.piece(ctx, tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("%+v, %v; want a refusal with %q", ack, err, tc.want)
			}
			if strings.Contains(name, "wrong signature beside") && cosignRan != 0 {
				t.Errorf("a wrong project signature fell through to cosign")
			}
			if strings.Contains(name, "no signature for") && !strings.Contains(err.Error(), "cosign is not installed") {
				t.Errorf("the reason does not say cosign is missing too: %v", err)
			}
			nothingArrived(t, u, dir, name)
		})
	}

	for name, tc := range map[string]struct {
		keys, repo string
		p          fleet.Update
		verified   string
	}{
		"signed with the only key, no cosign":       {pub1, testReleaseRepo, withSig(sums, nil, good), fleet.VerifiedProjectKey},
		"signed with the second key of two":         {pub1 + "," + pub2, testReleaseRepo, withSig(sums, nil, projectSign(priv2, sums)), fleet.VerifiedProjectKey},
		"a key and no repository":                   {pub1, "", withSig(sums, nil, good), fleet.VerifiedProjectKey},
		"no signature sent: cosign, as before":      {pub1, testReleaseRepo, withSig(sums, bundle, nil), fleet.VerifiedCosign},
		"no key: a signature sent is not looked at": {"", testReleaseRepo, withSig(sums, bundle, []byte("junk")), fleet.VerifiedCosign},
	} {
		t.Run(name, func(t *testing.T) {
			u, _ := releaseChild(t)
			carryKeys(t, tc.keys)
			childReleaseRepo = func() string { return tc.repo }
			if tc.verified == fleet.VerifiedProjectKey {
				childCosign = noCosign
			}
			ack, err := u.piece(ctx, tc.p)
			if err != nil || ack.Rule != "release" || ack.Verified != tc.verified || ack.Received != 0 {
				t.Fatalf("%+v, %v; want it taken with %s", ack, err, tc.verified)
			}
			p := fleet.Update{Version: "3.0.0", SHA256: shaOf(genuine), Size: int64(len(genuine)), Data: genuine[:100]}
			if ack, err := u.piece(ctx, p); err != nil || ack.Received != 100 {
				t.Fatalf("a piece: %+v, %v", ack, err)
			}
		})
	}
}

// The master says which check a machine passed, and nothing a machine
// makes up.
func TestRuleWordsSayWhichCheck(t *testing.T) {
	for verified, want := range map[string]string{
		fleet.VerifiedProjectKey: "with the project key its own Tenant carries",
		fleet.VerifiedCosign:     "the release workflow's signature",
		"":                       "against the release its own Tenant was built from",
		"\x1b[2Jpwned":           "against the release its own Tenant was built from",
	} {
		if got := ruleWords(string(fleet.UpdatesRelease), verified); !strings.Contains(got, want) || strings.Contains(got, "pwned") {
			t.Errorf("%q: %s", verified, got)
		}
	}
}
