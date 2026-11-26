// Command devbuild builds Tenant from this checkout and packs each build as
// the archive a release would publish, under a development version of its own
// (TEN-552):
//
//	<the last release's numbers>-dev.<UTC time of the build>.<commit>[.dirty]
//
// for example 0.3.0-dev.20261126193000.e56d774. The time makes every build a
// version of its own, so a machine and its master can tell two of them apart;
// the commit says what it was built from; .dirty says the tree had changes
// that were not committed. It sorts before the release candidates of the same
// numbers (selfupdate.Compare), so a release still updates over it.
//
// Run from anywhere in the repository:
//
//	go run ./scripts/devbuild                      # for this computer
//	go run ./scripts/devbuild darwin/arm64         # for another system
//	go run ./scripts/devbuild linux/amd64 windows/amd64
//	go run ./scripts/devbuild all                  # the six targets a release has
//	go run ./scripts/devbuild -o DIR darwin/arm64  # into DIR, not dist/dev
//
// Each archive is then sent to a fleet machine of its system, from the
// machine's master:
//
//	tenant fleet update <name> --build <archive>
//
// The machine has to be set to trust its master's hash (`tenant child install
// --updates master`, or `tenant child updates master` on it): a development
// build is no release, and no signature vouches for it. Like every development
// build it names no release repository and carries no project key, so `tenant
// update` leaves it alone unless forced.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"tenant/internal/selfupdate"
)

// releaseTargets are the systems and processors a release is built for
// (.goreleaser.yaml): the ones `tenant fleet update` knows an archive for.
var releaseTargets = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}

// What run needs of the computer. Tests replace them.
var (
	now = time.Now
	// gitOut runs git in dir and returns what it printed, trimmed.
	gitOut = func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && len(exit.Stderr) > 0 {
				return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(exit.Stderr)))
			}
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	// goBuild builds ./cmd/tenant of the checkout at root for one target, as
	// a release does (no cgo, no local paths in the binary), into out.
	goBuild = func(root, out, goos, goarch, ldflags string) error {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, "./cmd/tenant")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "devbuild:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("devbuild", flag.ContinueOnError)
	dest := fs.String("o", "", "the directory the archives are written to (default: dist/dev in the repository)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: go run ./scripts/devbuild [-o DIR] [all | SYSTEM/PROCESSOR ...]")
		fmt.Fprintln(fs.Output(), "  targets:", strings.Join(releaseTargets, " "), "(default: this computer's)")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	targets := fs.Args()
	switch {
	case len(targets) == 0:
		targets = []string{runtime.GOOS + "/" + runtime.GOARCH}
	case len(targets) == 1 && targets[0] == "all":
		targets = releaseTargets
	}
	for i, t := range targets {
		if !slices.Contains(releaseTargets, t) {
			return fmt.Errorf("%q is not a target: it is one of %s, or all", t, strings.Join(releaseTargets, ", "))
		}
		if slices.Contains(targets[:i], t) {
			return fmt.Errorf("%s is named twice", t)
		}
	}

	root, err := gitOut("", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w (run it inside the Tenant repository)", err)
	}
	root = filepath.FromSlash(root)
	commit, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	tag, _ := gitOut(root, "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*") // no tag yet: 0.0.0
	// What the binary is built from, and nothing else: notes and reports
	// lying about the checkout do not make a build dirty.
	changed, err := gitOut(root, "status", "--porcelain", "--", "cmd", "internal", "go.mod", "go.sum")
	if err != nil {
		return err
	}
	built := now().UTC()
	v := devVersion(tag, commit, changed != "", built)

	if *dest == "" {
		*dest = filepath.Join(root, "dist", "dev")
	}
	if err := os.MkdirAll(*dest, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "tenant-devbuild-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	const x = "-X tenant/internal/version."
	ldflags := fmt.Sprintf("-s -w %sVersion=%s %sCommit=%s %sBuildTime=%s %sDirty=%t", x, v, x, commit, x, built.Format(time.RFC3339), x, changed != "")
	fmt.Fprintf(out, "Tenant %s\n", v)
	for _, t := range targets {
		goos, goarch, _ := strings.Cut(t, "/")
		member := "tenant"
		if goos == "windows" {
			member = "tenant.exe"
		}
		bin := filepath.Join(tmp, goos+"_"+goarch, member)
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return err
		}
		if err := goBuild(root, bin, goos, goarch, ldflags); err != nil {
			return fmt.Errorf("the build for %s failed: %w", t, err)
		}
		archive := filepath.Join(*dest, selfupdate.AssetName(v, goos, goarch))
		if err := pack(archive, bin, member); err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		fmt.Fprintf(out, "  %-14s %s\n", t, archive)
	}
	fmt.Fprintln(out, "\nSend one to a fleet machine of its system, from the machine's master:")
	fmt.Fprintln(out, "  tenant fleet update <name> --build <archive>")
	fmt.Fprintln(out, "The machine must be set to trust its master's hash: `tenant child updates master`, run on it.")
	return nil
}

// devVersion is the version a development build is stamped with: the numbers
// of the last release tag (0.0.0 with none), then -dev, the time of the build
// to the second, the commit's first seven digits, and .dirty for a tree with
// changes not committed.
func devVersion(tag, commit string, dirty bool, built time.Time) string {
	base := "0.0.0"
	if v := strings.TrimPrefix(strings.TrimSpace(tag), "v"); v != "" {
		v, _, _ = strings.Cut(v, "+")
		v, _, _ = strings.Cut(v, "-")
		if _, ok := selfupdate.Compare(v, v); ok {
			base = v
		}
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	v := base + "-dev." + built.UTC().Format("20060102150405") + "." + commit
	if dirty {
		v += ".dirty"
	}
	return v
}

// pack writes the archive a release would carry the binary in: one entry,
// member, executable. A zip for Windows, a gzip'd tar otherwise, by the
// archive's name. The archive is whole or not there.
func pack(archive, bin, member string) (err error) {
	in, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := archive + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, archive)
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if strings.HasSuffix(archive, ".zip") {
		zw := zip.NewWriter(f)
		h := &zip.FileHeader{Name: member, Method: zip.Deflate, Modified: fi.ModTime()}
		h.SetMode(0o755)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, in); err != nil {
			return err
		}
		return zw.Close()
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, in); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
