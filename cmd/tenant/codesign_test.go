package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/procprotect"
	"tenant/internal/selfupdate"
)

// macOS signing (TEN-535, section 3 of the plan), tested on every system
// against a codesign that stands behind runCmd. The real one has not run
// against any of this: these tests hold the commands, their order and what
// each failure leaves behind, not what macOS does with a signed binary.

// What codesign prints for `-dvv` (on stderr; runCmd returns both streams).
const (
	dvvAdhocRuntime = `Executable=/Users/jane/.local/bin/tenant
Identifier=tenant
Format=Mach-O thin (arm64)
CodeDirectory v=20500 size=91534 flags=0x10002(adhoc,runtime) hashes=2855+0 location=embedded
Signature=adhoc
Info.plist=not bound
TeamIdentifier=not set
Runtime Version=14.0.0
Sealed Resources=none
Internal requirements count=0 size=12
`
	dvvLinkerSigned = `Executable=/Users/jane/src/tenant/tenant
Identifier=a.out
Format=Mach-O thin (arm64)
CodeDirectory v=20400 size=91522 flags=0x20002(adhoc,linker-signed) hashes=2855+0 location=embedded
Signature=adhoc
Info.plist=not bound
TeamIdentifier=not set
Sealed Resources=none
Internal requirements=none
`
	dvvAdhocNoRuntime = `Executable=/Users/jane/.local/bin/tenant
Identifier=tenant
Format=Mach-O thin (arm64)
CodeDirectory v=20400 size=91534 flags=0x2(adhoc) hashes=2855+0 location=embedded
Signature=adhoc
TeamIdentifier=not set
`
	dvvDeveloperID = `Executable=/Users/jane/.local/bin/tenant
Identifier=net.findtime.tenant
Format=Mach-O thin (arm64)
CodeDirectory v=20500 size=91534 flags=0x10000(runtime) hashes=2855+0 location=embedded
Signature size=9012
Authority=Developer ID Application: Example LLC (ABCDE12345)
Authority=Developer ID Certification Authority
Authority=Apple Root CA
Timestamp=18 Nov 2026 at 19:02:11
TeamIdentifier=ABCDE12345
Runtime Version=14.0.0
`
	dvvUnsigned = "/Users/jane/src/tenant/tenant: code object is not signed at all\n"
)

func TestParseSignature(t *testing.T) {
	exit1 := errors.New("exit status 1")
	for name, tc := range map[string]struct {
		out  string
		err  error
		want codeSignature
	}{
		"ad hoc with the hardened runtime": {dvvAdhocRuntime, nil, codeSignature{true, "ad hoc, signed on this machine"}},
		"the Go linker's own signature":    {dvvLinkerSigned, nil, codeSignature{false, "ad hoc, by the Go linker"}},
		"ad hoc without the flag":          {dvvAdhocNoRuntime, nil, codeSignature{false, "ad hoc, signed on this machine"}},
		"a Developer ID certificate":       {dvvDeveloperID, nil, codeSignature{true, "Developer ID Application: Example LLC (ABCDE12345)"}},
		"not signed":                       {dvvUnsigned, exit1, codeSignature{false, "not signed"}},
		"codesign is missing":              {"", errors.New("fork/exec /usr/bin/codesign: no such file or directory"), codeSignature{false, "its signature could not be read: fork/exec /usr/bin/codesign: no such file or directory"}},
		"codesign failed after printing":   {dvvAdhocRuntime, exit1, codeSignature{false, "its signature could not be read: Executable=/Users/jane/.local/bin/tenant"}},
		"no code directory line":           {"Executable=/x\nIdentifier=tenant\n", nil, codeSignature{false, "its signature could not be read: Executable=/x"}},
		// The flag is read from the code directory's own line, at the start of
		// a line, and from inside its brackets.
		"runtime elsewhere in the output": {"Executable=/x/flags=0x1(runtime)\nRuntime Version=14.0.0\nCodeDirectory v=20400 size=1 flags=0x2(adhoc) hashes=1+0\n", nil, codeSignature{false, "ad hoc, signed on this machine"}},
		"a flag that only contains it":    {"CodeDirectory v=20400 size=1 flags=0x2(adhoc,noruntime) hashes=1+0\n", nil, codeSignature{false, "ad hoc, signed on this machine"}},
		"a certificate with no name":      {"CodeDirectory v=20500 size=1 flags=0x10000(runtime) hashes=1+0\n", nil, codeSignature{true, "signed with a certificate"}},
	} {
		if got := parseSignature(tc.out, tc.err); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

// FuzzParseSignature: whatever codesign prints, reading it never panics, and
// the flag is believed only from output that names it on a code directory
// line and with codesign exiting 0.
func FuzzParseSignature(f *testing.F) {
	for _, seed := range []string{dvvAdhocRuntime, dvvLinkerSigned, dvvAdhocNoRuntime, dvvDeveloperID, dvvUnsigned, "",
		"CodeDirectory flags=0x(runtime)", "CodeDirectory v=1 flags=0x1(runtime", "x\nCodeDirectory v=1 flags=0xZ(runtime)\n",
		"CodeDirectory v=1 flags=0x1()\nAuthority=\n", "\rCodeDirectory v=1 flags=0x1(runtime)\r\n"} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, out string, failed bool) {
		var err error
		if failed {
			err = errors.New("exit status 1")
		}
		sig := parseSignature(out, err)
		if sig.Kind == "" {
			t.Fatalf("no kind for %q", out)
		}
		if !sig.Runtime {
			return
		}
		if failed {
			t.Fatalf("the flag believed from a codesign that failed: %q", out)
		}
		named := false
		for _, line := range strings.Split(out, "\n") {
			named = named || strings.HasPrefix(line, "CodeDirectory ") && strings.Contains(line, "flags=0x") && strings.Contains(line, "runtime")
		}
		if !named {
			t.Fatalf("the flag believed from %q", out)
		}
	})
}

// fakeCodesign is /usr/bin/codesign behind runCmd. A signature belongs to
// the file, not to its name: the fake remembers which files it signed (by
// identity, which a rename keeps), and -dvv answers for the file it is asked
// about. It leaves the bytes alone, so a signed helper binary still runs.
type fakeCodesign struct {
	t        *testing.T
	missing  bool              // the tool is not there
	failSign bool              // signing exits non-zero
	noFlag   bool              // it signs, without the hardened-runtime flag
	onVerify func(path string) // after a signed file's flags are read
	calls    [][]string        // codesign's own, in order
	signed   []os.FileInfo     // the files it signed
}

// has reports whether the file now at path is one this codesign signed.
func (f *fakeCodesign) has(path string) bool {
	now, err := os.Stat(path)
	return err == nil && slices.ContainsFunc(f.signed, func(was os.FileInfo) bool { return os.SameFile(was, now) })
}

func (f *fakeCodesign) run(argv []string) (string, error) {
	f.calls = append(f.calls, argv)
	exit1 := errors.New("exit status 1")
	if f.missing {
		return "", errors.New("fork/exec " + codesignTool + ": no such file or directory")
	}
	path := argv[len(argv)-1]
	switch strings.Join(argv[1:len(argv)-1], " ") {
	case "--force --sign - --options runtime": // ad hoc, the hardened runtime, no entitlements
		if f.failSign {
			return path + ": the codesign_allocate helper tool cannot be found or used", exit1
		}
		info, err := os.Stat(path)
		if err != nil {
			return path + ": No such file or directory", exit1
		}
		f.signed = append(f.signed, info)
		return "", nil
	case "-dvv":
		out := dvvAdhocRuntime
		switch _, err := os.Stat(path); {
		case err != nil:
			return path + ": No such file or directory", exit1
		case !f.has(path):
			return path + ": code object is not signed at all", exit1
		case f.noFlag:
			out = dvvAdhocNoRuntime
		}
		if f.onVerify != nil {
			f.onVerify(path)
		}
		return out, nil
	}
	f.t.Errorf("codesign was run with arguments this test does not know: %q", argv)
	return "", exit1
}

// install puts the fake behind runCmd; every other command gets other, or an
// error.
func (f *fakeCodesign) install(t *testing.T, other func(argv []string) (string, error)) *cmdRecorder {
	t.Helper()
	f.t = t
	rec := &cmdRecorder{reply: func(argv []string) (string, error) {
		if argv[0] == codesignTool {
			return f.run(argv)
		}
		if other != nil {
			return other(argv)
		}
		return "", errNoServiceManager
	}}
	rec.install(t)
	return rec
}

func (f *fakeCodesign) lines() string {
	var b strings.Builder
	for _, c := range f.calls {
		b.WriteString(strings.Join(c, " ") + "\n")
	}
	return b.String()
}

func TestInstallHardened(t *testing.T) {
	const original = "the binary as it was"
	ctx := context.Background()
	fixture := func(t *testing.T, content string) (exe, tmp string) {
		exe = filepath.Join(t.TempDir(), "tenant")
		if err := os.WriteFile(exe, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return exe, exe + ".signing"
	}
	holds := func(t *testing.T, path, want string) {
		t.Helper()
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("%s holds %q (%v), want %q", filepath.Base(path), b, err, want)
		}
	}
	gone := func(t *testing.T, path string) {
		t.Helper()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was left behind: %v", filepath.Base(path), err)
		}
	}

	t.Run("a copy is signed, checked, and only then moved into place", func(t *testing.T) {
		exe, tmp := fixture(t, original)
		if err := os.WriteFile(tmp, []byte("left by a run that was killed"), 0o600); err != nil {
			t.Fatal(err)
		}
		f := &fakeCodesign{}
		f.onVerify = func(path string) {
			if path == tmp && f.has(exe) { // the copy has its flag; the binary in place is still the old file
				t.Error("the binary in place was replaced before the copy's flag was checked")
			}
		}
		f.install(t, nil)
		if err := installHardened(ctx, exe); err != nil {
			t.Fatal(err)
		}
		holds(t, exe, original)
		if !f.has(exe) {
			t.Error("the binary in place is not the copy that was signed")
		}
		gone(t, tmp)
		want := codesignTool + " -dvv " + exe + "\n" +
			codesignTool + " --force --sign - --options runtime " + tmp + "\n" +
			codesignTool + " -dvv " + tmp + "\n"
		if got := f.lines(); got != want {
			t.Errorf("codesign was run as:\n%swant:\n%s", got, want)
		}
		if info, err := os.Stat(exe); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o755) {
			t.Errorf("the signed binary's mode: %v, %v; want the original's 0755", info.Mode().Perm(), err)
		}
	})
	t.Run("a binary that carries the flag is left alone", func(t *testing.T) {
		exe, tmp := fixture(t, original)
		f := &fakeCodesign{}
		f.install(t, nil)
		was, _ := os.Stat(exe)
		f.signed = append(f.signed, was) // signed before, by whoever
		if err := installHardened(ctx, exe); err != nil {
			t.Fatal(err)
		}
		if now, _ := os.Stat(exe); !os.SameFile(was, now) {
			t.Error("the binary was replaced")
		}
		holds(t, exe, original)
		gone(t, tmp)
		if got := f.lines(); got != codesignTool+" -dvv "+exe+"\n" {
			t.Errorf("codesign was run as:\n%s", got)
		}
	})
	for name, tc := range map[string]struct {
		fake *fakeCodesign
		want string // in the error
	}{
		"codesign is missing":                   {&fakeCodesign{missing: true}, "no such file or directory"},
		"signing fails":                         {&fakeCodesign{failSign: true}, "codesign --sign: " /* and what codesign said */},
		"the flag is not on the signed copy":    {&fakeCodesign{noFlag: true}, "the hardened-runtime flag is not on"},
		"the move fails (the copy disappeared)": {&fakeCodesign{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			exe, tmp := fixture(t, original)
			if tc.want == "" {
				tc.fake.onVerify = func(path string) {
					if path == tmp {
						_ = os.Remove(tmp)
					}
				}
			}
			tc.fake.install(t, nil)
			err := installHardened(ctx, exe)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one that says %q", err, tc.want)
			}
			holds(t, exe, original) // what was there is untouched
			if tc.fake.has(exe) {
				t.Error("the binary in place was replaced")
			}
			gone(t, tmp)
		})
	}
	t.Run("signing fails: the error carries what codesign said", func(t *testing.T) {
		exe, _ := fixture(t, original)
		(&fakeCodesign{failSign: true}).install(t, nil)
		if err := installHardened(ctx, exe); err == nil || !strings.Contains(err.Error(), "codesign_allocate helper tool cannot be found") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no binary", func(t *testing.T) {
		f := &fakeCodesign{}
		f.install(t, nil)
		exe := filepath.Join(t.TempDir(), "tenant")
		if err := installHardened(ctx, exe); err == nil || strings.Contains(f.lines(), "--sign") {
			t.Errorf("err = %v, codesign:\n%s", err, f.lines())
		}
		gone(t, exe+".signing")
	})
}

// On macOS `tenant update` signs the staged binary before it runs for the
// first time and before the swap; a failure leaves the running version as it
// was.
func TestUpdateSignsTheNewBinaryFirst(t *testing.T) {
	bin := buildFakeBin(t)
	archive := releaseArchive(t, bin)
	t.Setenv("TENANT_FAKE_VERSION", "9.9.9")
	prevPoll, prevHealth, prevSigns := updatePoll, updateHealth, signsBinaries
	updatePoll, updateHealth, signsBinaries = 5*time.Millisecond, 100*time.Millisecond, true
	t.Cleanup(func() { updatePoll, updateHealth, signsBinaries = prevPoll, prevHealth, prevSigns })

	for name, tc := range map[string]struct {
		fake    *fakeCodesign
		errText string
	}{
		"signed, then smoke-tested, then swapped": {&fakeCodesign{}, ""},
		"signing fails":                        {&fakeCodesign{failSign: true}, "codesign_allocate"},
		"the flag is not on the signed binary": {&fakeCodesign{noFlag: true}, "the hardened-runtime flag is not on"},
		"codesign is missing":                  {&fakeCodesign{missing: true}, "no such file or directory"},
	} {
		t.Run(name, func(t *testing.T) {
			releaseBuild(t, "1.0.0")
			f := newFakeRelease(t, "v9.9.9", archive)
			f.hubVersion = "9.9.9"
			in := newInstall(t)
			staged := filepath.Join(filepath.Dir(in.exe), ".tenant-update", exeName())
			tc.fake.onVerify = func(path string) {
				if b, _ := os.ReadFile(in.exe); path != staged || string(b) != "old binary" {
					t.Errorf("the flag was checked on %s with the installed binary holding %q; want the staged copy, and the old binary still in place", path, b[:min(len(b), 16)])
				}
			}
			tc.fake.install(t, nil)
			err := runUpdate(context.Background(), in.env(f, ""), updateOpts{yes: true})
			got, rerr := os.ReadFile(in.exe)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if tc.errText == "" {
				if err != nil || !bytes.Equal(got, bin) || !tc.fake.has(in.exe) {
					t.Fatalf("err = %v; the installed binary is the new one = %v, and the file that was signed = %v", err, bytes.Equal(got, bin), tc.fake.has(in.exe))
				}
				want := codesignTool + " --force --sign - --options runtime " + staged + "\n" + codesignTool + " -dvv " + staged + "\n"
				if tc.fake.lines() != want {
					t.Errorf("codesign was run as:\n%swant:\n%s", tc.fake.lines(), want)
				}
				if prev, _ := os.ReadFile(selfupdate.PrevPath(in.exe)); string(prev) != "old binary" {
					t.Errorf(".prev holds %q", prev)
				}
				return
			}
			for _, want := range []string{"not updated", "1.0.0 keeps running", "could not be signed with the hardened runtime", tc.errText} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to say %q", err, want)
				}
			}
			if string(got) != "old binary" || len(in.restarts) != 0 {
				t.Errorf("a failed signing touched the install: binary %q, restarts %v", got[:min(len(got), 16)], in.restarts)
			}
			for _, p := range []string{selfupdate.PrevPath(in.exe), preUpdateBackupPath(in.data, "1.0.0")} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("a failed signing left %s", p)
				}
			}
		})
	}
}

// On macOS `tenant service install` signs the binary before the service
// manager is asked for anything; a failure is said, and the service is
// installed with the binary as it was.
func TestServiceInstallSignsTheBinaryFirst(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(k, home) // a unit path under here, never the developer's own
	}
	prev := signsBinaries
	signsBinaries = true
	t.Cleanup(func() { signsBinaries = prev })
	noService := func(argv []string) (string, error) {
		if argv[0] == "sc" || argv[0] == "launchctl" && argv[1] == "print" {
			return "", errors.New("exit status 1")
		}
		return "", nil
	}
	for name, fake := range map[string]*fakeCodesign{"signed": {}, "signing fails": {failSign: true}} {
		t.Run(name, func(t *testing.T) {
			exe := filepath.Join(home, name+"-bin", "tenant")
			if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			spec, err := specFor(runtime.GOOS, exe, filepath.Join(home, "cfg"), filepath.Join(home, "data"), false)
			if err != nil {
				t.Fatal(err)
			}
			rec := fake.install(t, noService)
			out, _ := captureTenantStdout(t, func() error { return installService(context.Background(), spec) })
			signing, manager := 0, 0
			for i, c := range rec.calls {
				if c[0] == codesignTool {
					signing = i + 1
				} else if manager == 0 {
					manager = i + 1
				}
			}
			if signing == 0 || manager == 0 || manager < signing {
				t.Fatalf("commands, in order: %v; want codesign's, then the service manager's", rec.calls)
			}
			b, _ := os.ReadFile(exe)
			if fake.failSign {
				if string(b) != "binary" || fake.has(exe) || !strings.Contains(out, "warning: "+exe+" could not be signed with the hardened runtime") || !strings.Contains(out, "codesign_allocate") {
					t.Errorf("binary %q, said:\n%s", b, out)
				}
			} else if string(b) != "binary" || !fake.has(exe) || strings.Contains(out, "warning:") {
				t.Errorf("binary %q, signed = %v, said:\n%s", b, fake.has(exe), out)
			}
		})
	}
}

// Doctor reports the binary's flag and the kind of signature; an installed
// (release) binary without the flag is a warning and an audit finding, and a
// source build without it is neither.
func TestDoctorAndAuditReportTheHardenedRuntime(t *testing.T) {
	mac := procprotect.Report{Controls: []procprotect.Control{
		{ID: "darwin.deny-attach", Name: "debugger attach refused (PT_DENY_ATTACH)", OK: true, Note: "attach denied, not verified"},
		{ID: "darwin.core-limit", Name: "no core file (soft core limit 0)", OK: true}}}
	audit := func(sig *codeSignature, source bool) processProtectionAudit {
		a := newProcessProtectionAudit(mac, "darwin", nil, nil, "")
		a.Signature, a.SourceBuild = sig, source
		return a
	}
	finding := func(a processProtectionAudit) (securityFinding, bool) {
		report := securityAuditReport{ProcessProtection: a}
		evaluateSecurityAudit(&report)
		for _, f := range report.Findings {
			if f.ID == "process.darwin.hardened-runtime" {
				return f, true
			}
		}
		return securityFinding{}, false
	}
	section := func(a processProtectionAudit) string {
		out, _ := captureTenantStdout(t, func() error { printProcessProtection(a); return nil })
		return out
	}

	a := audit(&codeSignature{true, "ad hoc, signed on this machine"}, false)
	r := processProtectionCheck(a, "darwin")
	if _, found := finding(a); found || r.Status != statusOK || !strings.Contains(r.Detail, "on:  the hardened runtime on this binary, which makes macOS refuse a debugger outright (ad hoc, signed on this machine)") ||
		!strings.Contains(section(a), "on  darwin.hardened-runtime") {
		t.Errorf("signed: %+v, finding %v\n%s", r, found, section(a))
	}

	a = audit(&codeSignature{false, "ad hoc, by the Go linker"}, false) // a release binary nothing signed
	r = processProtectionCheck(a, "darwin")
	f, found := finding(a)
	if !found || !strings.Contains(f.Summary, "ad hoc, by the Go linker") || f.Remedy != signFix || r.Status != statusWarn ||
		!strings.Contains(r.Detail, "OFF: the hardened runtime on this binary (ad hoc, by the Go linker)") || !strings.Contains(r.Fix, "`tenant service install` signs this binary") ||
		!strings.Contains(section(a), "OFF darwin.hardened-runtime") {
		t.Errorf("an installed binary without the flag: %+v, finding %+v (%v)\n%s", r, f, found, section(a))
	}

	a = audit(&codeSignature{false, "ad hoc, by the Go linker"}, true)
	r = processProtectionCheck(a, "darwin")
	if _, found := finding(a); found || r.Status != statusOK || !strings.Contains(r.Detail, "a source build without the hardened runtime (ad hoc, by the Go linker): attach denied, not verified") {
		t.Errorf("a source build: %+v, finding %v", r, found)
	}

	a = audit(nil, false) // another system
	if r = processProtectionCheck(a, "darwin"); strings.Contains(r.Detail, "hardened runtime") || strings.Contains(section(a), "hardened") {
		t.Errorf("no signature read: %+v", r)
	}

	// The audit reads this binary's own signature, where binaries are signed.
	prev := signsBinaries
	t.Cleanup(func() { signsBinaries = prev })
	signsBinaries = true
	fake := &fakeCodesign{}
	fake.install(t, nil)
	releaseBuild(t, "1.0.0")
	got := auditProcessProtection(&commonFlags{})
	self, _ := os.Executable()
	self, _ = resolveExe(self)
	if got.Signature == nil || got.Signature.Runtime || got.Signature.Kind != "not signed" || got.SourceBuild || !got.unhardened() || fake.lines() != codesignTool+" -dvv "+self+"\n" {
		t.Errorf("this binary: %+v, source build %v, codesign:\n%s", got.Signature, got.SourceBuild, fake.lines())
	}
	signsBinaries = false
	if got := auditProcessProtection(&commonFlags{}); got.Signature != nil {
		t.Errorf("a signature read on a system that does not sign: %+v", got.Signature)
	}
}
