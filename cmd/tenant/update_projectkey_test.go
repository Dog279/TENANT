package main

// update_projectkey_test.go (TEN-542): `tenant update` on a binary that
// carries the project key takes only a release whose checksums.txt.sig
// verifies under one of its keys; a binary with none updates exactly as
// before. Refusals first.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/selfupdate"
	"tenant/internal/version"
)

// projectKey is a throwaway key made from a fixed seed, and its public text.
func projectKey(seed byte) (ed25519.PrivateKey, string) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	return priv, base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// projectSign is checksums.txt.sig over b, as scripts/releasekey writes it.
func projectSign(priv ed25519.PrivateKey, b []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, b)) + "\n")
}

// carryKeys makes this test binary carry keys (version.ReleaseKeys) until
// the test ends.
func carryKeys(t *testing.T, keys string) {
	t.Helper()
	prev := version.ReleaseKeys
	version.ReleaseKeys = keys
	t.Cleanup(func() { version.ReleaseKeys = prev })
}

// fakeCosignOnPath puts a file named cosign first on PATH, so HaveCosign is
// true, and answers its verify-blob through the runCmd seam: ok says
// whether the bundle verifies. Nothing is run.
func fakeCosignOnPath(t *testing.T, ok bool, ran *int) {
	t.Helper()
	dir := t.TempDir()
	name := "cosign"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := runCmd
	runCmd = func(_ context.Context, name string, args ...string) (string, error) {
		if name != "cosign" || args[0] != "verify-blob" {
			return prev(context.Background(), name, args...)
		}
		*ran++
		if !ok {
			return "", errorString("no matching signatures")
		}
		return "Verified OK", nil
	}
	t.Cleanup(func() { runCmd = prev })
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestUpdateProjectKey(t *testing.T) {
	bin := buildFakeBin(t)
	archive := releaseArchive(t, bin)
	t.Setenv("TENANT_FAKE_VERSION", "9.9.9")
	prevPoll, prevHealth := updatePoll, updateHealth
	updatePoll, updateHealth = 5*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { updatePoll, updateHealth = prevPoll, prevHealth })
	priv1, pub1 := projectKey(1)
	priv2, pub2 := projectKey(2)
	priv3, _ := projectKey(3)

	for _, tc := range []struct {
		name   string
		keys   string // version.ReleaseKeys
		prep   func(t *testing.T, f *fakeRelease)
		cosign string // "" none on PATH, "ok", "bad"
		code   int
		said   string // in stdout, or in the error when code != 0
	}{
		{name: "tampered checksums", keys: pub1, code: 1, said: "does not verify",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte(f.sums))
				f.sums += strings.Repeat("0", 64) + "  tenant_9.9.9_plan9_x86_64.tar.gz\n" // still lists the archive rightly
			}},
		{name: "a signature by another key", keys: pub1 + "," + pub2, code: 1, said: "does not verify",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv3, []byte(f.sums))
			}},
		{name: "a signature for another file", keys: pub1, code: 1, said: "does not verify",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte("checksums of another release\n"))
			}},
		{name: "no signature for a binary that carries a key", keys: pub1, code: 1, said: "carries no project signature"},
		{name: "a malformed signature", keys: pub1, code: 1, said: "malformed",
			prep: func(_ *testing.T, f *fakeRelease) {
				sig := projectSign(priv1, []byte(f.sums))
				sig[len(sig)-1] = ' ' // a space where the newline was
				f.assets[selfupdate.SignatureAsset] = sig
			}},
		{name: "an over-long signature", keys: pub1, code: 1, said: "checksums.txt.sig is larger than Tenant takes",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = bytes.Repeat([]byte("A"), 1<<16)
			}},
		{name: "a key that does not read", keys: pub1 + ",", code: 1, said: "project key does not read",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte(f.sums))
			}},
		{name: "an over-long key list", keys: strings.Repeat(pub1, 4), code: 1, said: "project key does not read",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte(f.sums))
			}},
		{name: "a good signature and a bad cosign signature", keys: pub1, cosign: "bad", code: 1, said: "cosign verify-blob",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte(f.sums))
				f.assets["checksums.txt.sigstore.json"] = []byte("{}")
			}},

		{name: "signed with the only key, no cosign", keys: pub1,
			said: "verified checksum and the project key's signature (the release carries no signature bundle for cosign)",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte(f.sums))
			}},
		{name: "signed with the second key of two", keys: pub1 + "," + pub2,
			said: "verified checksum and the project key's signature",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv2, []byte(f.sums))
			}},
		{name: "a bundle but no cosign on the machine", keys: pub1,
			said: "verified checksum and the project key's signature (install cosign to verify the release workflow's signature as well)",
			prep: func(t *testing.T, f *fakeRelease) {
				t.Setenv("PATH", t.TempDir())
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte(f.sums))
				f.assets["checksums.txt.sigstore.json"] = []byte("{}")
			}},
		{name: "both signatures verify", keys: pub1, cosign: "ok",
			said: "verified checksum, the project key's signature and the release workflow's signature",
			prep: func(_ *testing.T, f *fakeRelease) {
				f.assets[selfupdate.SignatureAsset] = projectSign(priv1, []byte(f.sums))
				f.assets["checksums.txt.sigstore.json"] = []byte("{}")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releaseBuild(t, "1.0.0")
			carryKeys(t, tc.keys)
			f := newFakeRelease(t, "v9.9.9", archive)
			f.hubVersion = "9.9.9"
			ran := 0
			if tc.cosign != "" {
				fakeCosignOnPath(t, tc.cosign == "ok", &ran)
			}
			if tc.prep != nil {
				tc.prep(t, f)
			}
			in := newInstall(t)
			err := runUpdate(context.Background(), in.env(f, ""), updateOpts{yes: true})
			if exitCodeOf(err) != tc.code {
				t.Fatalf("exit %d (%v), want %d\n%s", exitCodeOf(err), err, tc.code, in.out.String())
			}
			said := in.out.String()
			if err != nil {
				said = err.Error()
			}
			if !strings.Contains(said, tc.said) {
				t.Errorf("said:\n%s\nwant %q", said, tc.said)
			}
			got, _ := os.ReadFile(in.exe)
			if swapped := bytes.Equal(got, bin); swapped != (tc.code == 0) {
				t.Errorf("binary swapped = %v", swapped)
			}
			if _, err := os.Stat(preUpdateBackupPath(in.data, "1.0.0")); (err == nil) != (tc.code == 0) {
				t.Errorf("backup written = %v: a refused release must change nothing", err == nil)
			}
			if tc.cosign != "" && ran != 1 {
				t.Errorf("cosign ran %d times, want once: both are checked when both can be", ran)
			}
		})
	}
}

// A binary with no key updates exactly as before: each scenario of
// TestUpdateFlow's kind says and leaves the same with and without a
// project signature on the release, and a bad one changes nothing.
func TestUpdateNoKeyUnchanged(t *testing.T) {
	bin := buildFakeBin(t)
	archive := releaseArchive(t, bin)
	t.Setenv("TENANT_FAKE_VERSION", "9.9.9")
	prevPoll, prevHealth := updatePoll, updateHealth
	updatePoll, updateHealth = 5*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { updatePoll, updateHealth = prevPoll, prevHealth })
	priv, _ := projectKey(1)

	type outcome struct {
		code               int
		out, err           string
		swapped, prev, bak bool
		restarts           []bool
	}
	run := func(t *testing.T, running string, o updateOpts, noService, badSums bool, sig []byte) outcome {
		releaseBuild(t, running)
		carryKeys(t, "")
		f := newFakeRelease(t, "v9.9.9", archive)
		f.hubVersion, f.badSums = "9.9.9", badSums
		if sig != nil {
			f.assets[selfupdate.SignatureAsset] = sig
		}
		in := newInstall(t)
		u := in.env(f, "")
		if noService {
			u.restart = nil
		}
		err := runUpdate(context.Background(), u, o)
		root := filepath.Dir(in.data)
		oc := outcome{code: exitCodeOf(err), out: strings.ReplaceAll(in.out.String(), root, "<root>"), restarts: in.restarts}
		if err != nil {
			oc.err = strings.ReplaceAll(err.Error(), root, "<root>")
		}
		got, _ := os.ReadFile(in.exe)
		oc.swapped = bytes.Equal(got, bin)
		_, perr := os.Stat(selfupdate.PrevPath(in.exe))
		_, berr := os.Stat(preUpdateBackupPath(in.data, running))
		oc.prev, oc.bak = perr == nil, berr == nil
		return oc
	}
	for _, sc := range []struct {
		name      string
		running   string
		opts      updateOpts
		noService bool
		badSums   bool
	}{
		{"happy path", "1.0.0", updateOpts{yes: true}, false, false},
		{"no service", "1.0.0", updateOpts{yes: true, now: true}, true, false},
		{"checksum mismatch", "1.0.0", updateOpts{yes: true}, false, true},
		{"dev build with --force", "0.0.1", updateOpts{yes: true, force: true}, false, false},
		{"--check", "1.0.0", updateOpts{check: true}, false, false},
	} {
		t.Run(sc.name, func(t *testing.T) {
			base := run(t, sc.running, sc.opts, sc.noService, sc.badSums, nil)
			for what, sig := range map[string][]byte{"a good project signature": nil, "a junk project signature": []byte("junk")} {
				if sig == nil {
					sig = projectSign(priv, []byte("whatever"))
				}
				got := run(t, sc.running, sc.opts, sc.noService, sc.badSums, sig)
				if got.code != base.code || got.out != base.out || got.err != base.err || got.swapped != base.swapped ||
					got.prev != base.prev || got.bak != base.bak || !slices.Equal(got.restarts, base.restarts) {
					t.Errorf("with %s:\n%+v\nwithout:\n%+v", what, got, base)
				}
			}
			if sc.name == "happy path" && !strings.Contains(base.out, "verified checksum only (the release carries no signature bundle)\n") {
				t.Errorf("the happy path said:\n%s", base.out)
			}
		})
	}
}
