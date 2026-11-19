package main

// codesign.go (TEN-535, section 3 of
// docs/plans/process-protection-plan.md): on macOS the installed binary
// carries the hardened-runtime flag, which makes the system refuse a debugger
// outright. `tenant service install` and `tenant update` put it there with a
// local (ad hoc) signature and no entitlements: a copy is signed, the flag is
// checked on the copy, and only then is the copy moved into place, so the
// binary that runs is never one that is half signed. scripts/install.sh does
// the same for the first install. A failure leaves what was there untouched
// and is reported; whether Tenant then starts is the acknowledgement flow's
// decision (protectiongate.go), from what the binary that is there can apply.
//
// The file has no build tag. Nothing here runs on another system
// (signsBinaries), and its tests run on every system against a codesign that
// stands behind runCmd. None of it has run against the real codesign: see the
// macOS section of docs/serve-runbook.md for what to check on a Mac.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// codesignTool is part of macOS. It is named by its full path, so a codesign
// earlier on PATH is not what signs or what is believed.
const codesignTool = "/usr/bin/codesign"

// signsBinaries: this system has the hardened runtime. A variable, so the
// tests of the update and the service install run the macOS path anywhere.
var signsBinaries = runtime.GOOS == "darwin"

// codeSignature is what codesign says about a binary.
type codeSignature struct {
	// Runtime: the hardened-runtime flag is on.
	Runtime bool `json:"hardened_runtime"`
	// Kind is the signature in plain words: ad hoc, the Go linker's, the
	// certificate's name, not signed, or why it could not be read.
	Kind string `json:"kind"`
}

var (
	// The code directory's line in `codesign -dvv`, on stderr:
	//   CodeDirectory v=20500 size=… flags=0x10002(adhoc,runtime) hashes=…
	codeFlagsRe = regexp.MustCompile(`(?m)^CodeDirectory [^\n]*\bflags=0x[0-9a-fA-F]+\(([^)\n]*)\)`)
	// The certificate a binary was signed with, first in its chain:
	//   Authority=Developer ID Application: Example LLC (ABCDE12345)
	authorityRe = regexp.MustCompile(`(?m)^Authority=([^\r\n]+)`)
)

// parseSignature reads what `codesign -dvv` printed, and how it ended.
func parseSignature(out string, err error) codeSignature {
	m := codeFlagsRe.FindStringSubmatch(out)
	if err != nil || m == nil {
		if strings.Contains(out, "not signed at all") {
			return codeSignature{Kind: "not signed"}
		}
		why, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
		if why = strings.TrimSpace(why); why == "" && err != nil {
			why = err.Error()
		}
		if len(why) > 200 {
			why = why[:200]
		}
		return codeSignature{Kind: "its signature could not be read: " + why}
	}
	flags := strings.Split(m[1], ",")
	sig := codeSignature{Runtime: slices.Contains(flags, "runtime")}
	switch a := authorityRe.FindStringSubmatch(out); {
	case slices.Contains(flags, "linker-signed"):
		sig.Kind = "ad hoc, by the Go linker"
	case slices.Contains(flags, "adhoc"):
		sig.Kind = "ad hoc, signed on this machine"
	case a != nil && strings.TrimSpace(a[1]) != "":
		sig.Kind = strings.TrimSpace(a[1])
	default:
		sig.Kind = "signed with a certificate"
	}
	return sig
}

// readSignature asks codesign about the binary at path.
func readSignature(ctx context.Context, path string) codeSignature {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return parseSignature(runCmd(ctx, codesignTool, "-dvv", path))
}

// signHardened signs the binary at path where it is, ad hoc, with the
// hardened runtime and no entitlements, and checks that the flag is on it.
// path is a copy that nothing runs yet.
func signHardened(ctx context.Context, path string) error {
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if out, err := runCmd(sctx, codesignTool, "--force", "--sign", "-", "--options", "runtime", path); err != nil {
		return cmdErr("codesign --sign", out, err)
	}
	if sig := readSignature(ctx, path); !sig.Runtime {
		return fmt.Errorf("codesign ran, but the hardened-runtime flag is not on %s (%s)", path, sig.Kind)
	}
	return nil
}

// installHardened makes the binary at exe one that carries the
// hardened-runtime flag. One that has it is left alone, whoever signed it.
// Otherwise a copy beside it is signed and checked, and only then renamed
// over it: exe is untouched unless every step worked.
func installHardened(ctx context.Context, exe string) error {
	if readSignature(ctx, exe).Runtime {
		return nil
	}
	tmp := exe + ".signing" // beside it, so the rename is one step on one filesystem
	if err := copyExecutable(exe, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp) // already gone once it is renamed
	if err := signHardened(ctx, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, exe)
}

// copyExecutable copies src to a new file dst, with src's mode. Whatever was
// at dst is removed first, and a dst that appears in between is not followed.
func copyExecutable(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())

	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}

// unsignedNote is what the operator is told when the binary a service runs
// could not be given the flag.
func unsignedNote(exe string, why error) string {
	return fmt.Sprintf("warning: %s could not be signed with the hardened runtime (%v). It is left as it was: a debugger is kept out by PT_DENY_ATTACH at start alone (attach denied, not verified). `tenant doctor` shows it", exe, why)
}
