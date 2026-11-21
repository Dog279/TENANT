package main

// childrelease_test.go: whose word an update is taken on (decision A71). By
// default a child verifies the release itself, offline, against the release
// identity built into its own binary; with "updates": "master" it takes the
// master's hash, as before. Refusals first.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"tenant/internal/fleet"
	"tenant/internal/selfupdate"
)

// testReleaseRepo is the release identity the test child's binary carries.
const testReleaseRepo = "acme/tenant"

// fakeSign is a "signature bundle" over checksums by repo's release
// workflow, as fakeCosign checks it: the stand-in for Sigstore, which
// cannot be reached from a test.
func fakeSign(repo string, checksums []byte) []byte {
	return []byte("signed-by " + repo + " " + shaOf(checksums))
}

// fakeCosign is cosign verify-blob for fakeSign's bundles: it reads the
// arguments the child built (CosignArgs), and the bundle verifies only if it
// signs exactly the checksums file and its signer matches the identity the
// child asked for. It insists on --offline.
func fakeCosign(_ context.Context, args ...string) error {
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	if args[0] != "verify-blob" || !slices.Contains(args, "--offline") {
		return fmt.Errorf("not an offline verify-blob: %q", args)
	}
	bundle, err := os.ReadFile(flag("--bundle"))
	if err != nil {
		return err
	}
	sums, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		return err
	}
	f := strings.Fields(string(bundle))
	if len(f) != 3 || f[0] != "signed-by" || f[2] != shaOf(sums) {
		return errors.New("no matching signatures")
	}
	re, err := regexp.Compile(flag("--certificate-identity-regexp"))
	if err != nil || !re.MatchString("https://github.com/"+f[1]+"/.github/workflows/release.yml@refs/tags/v3.0.0") {
		return errors.New("none of the expected identities matched what was in the certificate")
	}
	return nil
}

// releaseChild is a child's updater on the default rule, with a binary built
// from testReleaseRepo and the fake cosign.
func releaseChild(t *testing.T) (*childUpdater, string) {
	t.Helper()
	u, dir := childUpdaterRig(t)
	rigChild(t, dir, "")
	prevRepo, prevCosign := childReleaseRepo, childCosign
	childReleaseRepo, childCosign = func() string { return testReleaseRepo }, fakeCosign
	t.Cleanup(func() { childReleaseRepo, childCosign = prevRepo, prevCosign })
	return u, dir
}

// release is a release of version v for this machine: its archive's name,
// a checksums.txt listing archive's hash (and another target's), and the
// bundle repo signed it with.
func release(v string, archive []byte, repo string) (asset string, checksums, bundle []byte) {
	asset = selfupdate.AssetName(v, runtime.GOOS, runtime.GOARCH)
	checksums = []byte(shaOf(archive) + "  " + asset + "\n" + strings.Repeat("0", 64) + "  tenant_" + v + "_plan9_x86_64.tar.gz\n")
	return asset, checksums, fakeSign(repo, checksums)
}

// opening is the piece with no data that opens a send, with the material.
func opening(v string, archive []byte, asset string, checksums, bundle []byte) fleet.Update {
	return fleet.Update{Version: v, SHA256: shaOf(archive), Size: int64(len(archive)), Asset: asset, Checksums: checksums, Bundle: bundle}
}

// nothingArrived checks a refused update left nothing on the child.
func nothingArrived(t *testing.T, u *childUpdater, dir, what string) {
	t.Helper()
	noStage(t, u, what)
	entries, _ := os.ReadDir(filepath.Join(childDataDir(dir), "update"))
	for _, e := range entries {
		t.Errorf("%s: %s is left in the update folder", what, e.Name())
	}
}

func TestChildReleaseRuleRefusals(t *testing.T) {
	ctx := context.Background()
	genuine := bytes.Repeat([]byte("genuine "), 64)
	tampered := append(bytes.Repeat([]byte("genuine "), 63), []byte("evil!!!!")...)
	asset, sums, bundle := release("3.0.0", genuine, testReleaseRepo)
	_, forgedSums, _ := release("3.0.0", tampered, testReleaseRepo)
	_, _, otherBundle := release("3.0.0", genuine, "evil/tenant")
	_, onlyOther, _ := release("3.0.0", genuine, testReleaseRepo)
	onlyOther = bytes.ReplaceAll(onlyOther, []byte(asset), []byte("tenant_3.0.0_plan9_arm.tar.gz"))
	onlyOtherBundle := fakeSign(testReleaseRepo, onlyOther)
	otherOS := selfupdate.AssetName("3.0.0", "plan9", runtime.GOARCH)

	for name, tc := range map[string]struct {
		p    fleet.Update
		want string
	}{
		// The case the decision exists for: a master (or its disk) changed the
		// archive and names the changed archive's hash.
		"a tampered archive with the master's hash matching": {opening("3.0.0", tampered, asset, sums, bundle), "list another SHA-256 for " + asset},
		"checksums changed to list the tampered archive":     {opening("3.0.0", tampered, asset, forgedSums, bundle), "signature does not verify as acme/tenant"},
		"a valid release signed by another identity":         {opening("3.0.0", genuine, asset, sums, otherBundle), "signature does not verify as acme/tenant"},
		"checksums that do not list this archive":            {opening("3.0.0", genuine, asset, onlyOther, onlyOtherBundle), "list no " + asset},
		"another system's archive":                           {opening("3.0.0", genuine, otherOS, sums, bundle), "not " + asset},
		"another version's name":                             {opening("2.9.0", genuine, asset, sums, bundle), "this machine's for version"},
		"a release with no signature bundle":                 {opening("3.0.0", genuine, asset, sums, nil), "carries no signature bundle"},
		"no material at all (a master from before)":          {fleet.Update{Version: "3.0.0", SHA256: shaOf(genuine), Size: int64(len(genuine))}, "sent no release checksums"},
		"the archive's first piece, with no material":        {fleet.Update{Version: "3.0.0", SHA256: shaOf(tampered), Size: int64(len(tampered)), Data: tampered[:100]}, "sent no release checksums"},
	} {
		t.Run(name, func(t *testing.T) {
			u, dir := releaseChild(t)
			ack, err := u.piece(ctx, tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("%+v, %v; want a refusal with %q", ack, err, tc.want)
			}
			nothingArrived(t, u, dir, name)
			// And the archive's pieces after a refused opening are refused too.
			if tc.p.Data == nil {
				p := tc.p
				p.Asset, p.Checksums, p.Bundle, p.Data = "", nil, nil, genuine[:10]
				if _, err := u.piece(ctx, p); err == nil {
					t.Fatal("a piece after a refused opening was taken")
				}
				nothingArrived(t, u, dir, name+", then a piece")
			}
		})
	}

	t.Run("a development build", func(t *testing.T) {
		u, dir := releaseChild(t)
		childReleaseRepo = func() string { return "" }
		if _, err := u.piece(ctx, opening("3.0.0", genuine, asset, sums, bundle)); err == nil || !strings.Contains(err.Error(), "names no release") ||
			!strings.Contains(err.Error(), "tenant child updates master") {
			t.Fatalf("%v", err)
		}
		nothingArrived(t, u, dir, "a development build")
		c, _ := fleet.LoadChild(fleet.ChildPath(dir))
		if line := updatesLine(c); !strings.Contains(line, "refused") || !strings.Contains(line, "tenant child updates master") {
			t.Fatalf("status says %q", line)
		}
	})

	t.Run("no cosign on the machine", func(t *testing.T) {
		u, dir := releaseChild(t)
		childCosign = func(context.Context, ...string) error { return errors.New("cosign is not installed on this machine") }
		if _, err := u.piece(ctx, opening("3.0.0", genuine, asset, sums, bundle)); err == nil || !strings.Contains(err.Error(), "cosign is not installed") {
			t.Fatalf("%v", err)
		}
		nothingArrived(t, u, dir, "no cosign")
	})

	// What a default child refuses, a child set to trust its master takes:
	// the tampered archive with the master's hash.
	t.Run("a master child takes the master's word", func(t *testing.T) {
		u, dir := childUpdaterRig(t)
		ack, err := u.piece(ctx, opening("3.0.0", tampered, asset, sums, bundle))
		if err != nil || ack.Rule != "master" {
			t.Fatalf("%+v, %v", ack, err)
		}
		p := fleet.Update{Version: "3.0.0", SHA256: shaOf(tampered), Size: int64(len(tampered)), Data: tampered[:100]}
		if ack, err := u.piece(ctx, p); err != nil || ack.Received != 100 {
			t.Fatalf("a piece: %+v, %v", ack, err)
		}
		_ = dir
	})

	// The default child takes a genuine release's pieces once its opening
	// verified, and says which rule it applied.
	t.Run("a genuine release's pieces are taken after the opening verifies", func(t *testing.T) {
		u, _ := releaseChild(t)
		ack, err := u.piece(ctx, opening("3.0.0", genuine, asset, sums, bundle))
		if err != nil || ack.Rule != "release" || ack.Received != 0 {
			t.Fatalf("the opening: %+v, %v", ack, err)
		}
		p := fleet.Update{Version: "3.0.0", SHA256: shaOf(genuine), Size: int64(len(genuine)), Data: genuine[:100]}
		if ack, err := u.piece(ctx, p); err != nil || ack.Received != 100 {
			t.Fatalf("a piece: %+v, %v", ack, err)
		}
		// Another archive's piece, which no opening verified, is refused.
		q := fleet.Update{Version: "3.0.0", SHA256: shaOf(tampered), Size: int64(len(tampered)), Data: tampered[:100]}
		if _, err := u.piece(ctx, q); err == nil {
			t.Fatal("a piece of an archive no opening verified was taken")
		}
	})
}

// A default child, end to end in this process: a genuine release of a
// Tenant that runs here is verified, taken, and staged.
func TestChildReleaseRuleStagesAGenuineRelease(t *testing.T) {
	noSigning(t) // not about the macOS signing step (see noSigning)
	if testing.Short() {
		t.Skip("runs a build of this test binary")
	}
	helperCommands(t)
	build := e2eBuild(t, filepath.Join(realTemp(t), "b"), "3.0.0", "")
	raw, err := os.ReadFile(build)
	if err != nil {
		t.Fatal(err)
	}
	member := exeName()
	archive := archiveOf(t, runtime.GOOS == "windows", map[string][]byte{member: raw})
	u, _ := releaseChild(t)
	asset, sums, bundle := release("3.0.0", archive, testReleaseRepo)
	if _, err := u.piece(context.Background(), opening("3.0.0", archive, asset, sums, bundle)); err != nil {
		t.Fatal(err)
	}
	ack, err := send(t, u, "3.0.0", archive, fleet.MaxUpdateChunk)
	if err != nil || !ack.Applying || ack.Rule != "release" {
		t.Fatalf("%+v, %v", ack, err)
	}
	if s := <-u.ready; s.to != "3.0.0" || e2eMarkOf(t, s.binary) != "3.0.0" {
		t.Fatalf("staged %+v", s)
	}
}

// The rule is chosen on the machine: at install, or with `tenant child
// updates`, which needs the child stopped, records the change in the audit
// log, and is what status shows. Rehome keeps it.
func TestChildUpdatesSetting(t *testing.T) {
	prevRepo := childReleaseRepo
	childReleaseRepo = func() string { return testReleaseRepo }
	t.Cleanup(func() { childReleaseRepo = prevRepo })
	m := newFleetMaster(t)
	dir, digits, said := enrollChild(t, m, "box", childInstallOpts{updates: "master"})
	if !strings.Contains(said, "updates:  master") {
		t.Fatalf("install said:\n%s", said)
	}
	if c, _ := fleet.LoadChild(fleet.ChildPath(dir)); c.Updates != fleet.UpdatesMaster {
		t.Fatalf("child.json: %+v", c)
	}
	if _, _, err := enrollWith(t, m.code("box2"), childInstallOpts{updates: "anyone"}); exitCode(err) != 2 || !strings.Contains(err.Error(), "release") {
		t.Fatalf("--updates anyone: %v", err)
	}
	mach := m.confirm("box", digits)
	stop := startChild(t, dir)
	m.awaitOnline(mach.ID, true)
	var out bytes.Buffer
	if err := childUpdates(&out, dir, "release"); exitCode(err) != 2 || !strings.Contains(err.Error(), "a child process is running") {
		t.Fatalf("updates beside a running child: %v", err)
	}
	stop()
	m.awaitOnline(mach.ID, false)
	before, _ := fleet.VerifyAudit(dir)
	out.Reset()
	if err := childUpdates(&out, dir, "release"); err != nil || !strings.Contains(out.String(), "updates:  release: an update is taken only if its signature verifies here as acme/tenant") {
		t.Fatalf("updates release: %v\n%s", err, out.String())
	}
	if c, _ := fleet.LoadChild(fleet.ChildPath(dir)); c.Updates != "" {
		t.Fatalf("the default is written as nothing: %+v", c)
	}
	if after, err := fleet.VerifyAudit(dir); err != nil || after.Records != before.Records+1 {
		t.Fatalf("the audit log: %+v %v", after, err)
	}
	if r := lastAuditRecord(t, dir); r.Event != fleet.AuditUpdates || r.From != "master" || r.To != "release" || r.By == "" {
		t.Fatalf("the audit record: %+v", r)
	}
	if st, _ := status(dir); !strings.Contains(st, "updates:  release") {
		t.Fatalf("status:\n%s", st)
	}
	if err := childUpdates(&out, dir, "master"); err != nil {
		t.Fatal(err)
	}
	// Rehome keeps it.
	b := newFleetMaster(t)
	if _, err := rehome(t, dir, b.code("box")); err != nil {
		t.Fatal(err)
	}
	if c, _ := fleet.LoadChild(fleet.ChildPath(dir)); c.Updates != fleet.UpdatesMaster {
		t.Fatalf("after rehome: %+v", c)
	}
}

// The update rule is never set over the link: the one link method that
// writes child.json (fleet/addresses, client.addresses) changes nothing but
// the master's addresses, and the only other writers are the commands on the
// machine (TestChildRehomeIsStartedOnlyOnTheMachine pins those).
func TestChildUpdateRuleIsNotSetOverTheLink(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "..", "internal", "fleet", "client.go"), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "addresses" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "child" && sel.Sel.Name != "MasterAddresses" {
						t.Errorf("fleet/addresses sets child.%s", sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("client.addresses is not where it was: check what writes child.json over the link")
	}
}
