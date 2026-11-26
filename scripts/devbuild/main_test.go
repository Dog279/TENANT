package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/selfupdate"
)

// A development build's version: the last release's numbers, the time of the
// build, the commit, and whether the tree was clean. It sorts before the
// release candidates of the same numbers and after an earlier build.
func TestDevVersion(t *testing.T) {
	at := time.Date(2026, 11, 26, 19, 30, 0, 0, time.FixedZone("PST", -8*3600)) // 03:30 UTC the next day
	for _, tc := range []struct {
		tag, commit string
		dirty       bool
		want        string
	}{
		{"v0.3.0-rc.2", "e56d7744a1b2c3d4e5f60718293a4b5c6d7e8f90", false, "0.3.0-dev.20261127033000.e56d774"},
		{"v0.3.0-rc.2", "e56d7744a1b2c3d4e5f60718293a4b5c6d7e8f90", true, "0.3.0-dev.20261127033000.e56d774.dirty"},
		{"v0.2.0", "0a1b2c3", false, "0.2.0-dev.20261127033000.0a1b2c3"},
		{"", "0a1b2c3d", false, "0.0.0-dev.20261127033000.0a1b2c3"},
		{"not-a-version", "0a1b2c3d", false, "0.0.0-dev.20261127033000.0a1b2c3"},
	} {
		if got := devVersion(tc.tag, tc.commit, tc.dirty, at); got != tc.want {
			t.Errorf("devVersion(%q, %q, %v) = %q, want %q", tc.tag, tc.commit, tc.dirty, got, tc.want)
		}
	}
	dev := devVersion("v0.3.0-rc.2", "e56d774", false, at)
	for other, want := range map[string]int{
		"0.3.0-rc.1": -1, "0.3.0-rc.3": -1, "0.3.0": -1, "0.2.0": 1,
		devVersion("v0.3.0-rc.2", "0a1b2c3", false, at.Add(time.Second)): -1,
		dev: 0,
	} {
		if c, ok := selfupdate.Compare(dev, other); !ok || c != want {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d", dev, other, c, ok, want)
		}
	}
}

// run builds each target named and packs it as the archive a release would
// carry: named so that `tenant fleet update --build` reads the version and
// the system back from it, and holding the binary under the name `tenant
// child` takes it out by. The build itself is stood in for.
func TestRunPacksEachTarget(t *testing.T) {
	prevNow, prevGit, prevBuild := now, gitOut, goBuild
	t.Cleanup(func() { now, gitOut, goBuild = prevNow, prevGit, prevBuild })
	root, dest := t.TempDir(), t.TempDir()
	now = func() time.Time { return time.Date(2026, 11, 26, 19, 30, 0, 0, time.UTC) }
	status := ""
	gitOut = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "rev-parse":
			if args[1] == "--show-toplevel" {
				return filepath.ToSlash(root), nil
			}
			return "e56d7744a1b2c3d4e5f60718293a4b5c6d7e8f90", nil
		case "describe":
			return "v0.3.0-rc.2", nil
		case "status":
			return status, nil
		}
		t.Errorf("git %v", args)
		return "", nil
	}
	var flags []string
	goBuild = func(dir, out, goos, goarch, ldflags string) error {
		if dir != root {
			t.Errorf("built in %s, not the repository %s", dir, root)
		}
		flags = append(flags, ldflags)
		return os.WriteFile(out, []byte("a tenant for "+goos+"/"+goarch), 0o755)
	}

	var out bytes.Buffer
	if err := run([]string{"-o", dest, "linux/amd64", "windows/arm64"}, &out); err != nil {
		t.Fatal(err)
	}
	const v = "0.3.0-dev.20261126193000.e56d774"
	if !strings.Contains(out.String(), "Tenant "+v) || !strings.Contains(out.String(), "--build") {
		t.Errorf("it said:\n%s", out.String())
	}
	for _, f := range flags {
		for _, want := range []string{"tenant/internal/version.Version=" + v, "version.Commit=e56d7744a1b2c3d4e5f60718293a4b5c6d7e8f90", "version.Dirty=false", "version.BuildTime=2026-11-26T19:30:00Z"} {
			if !strings.Contains(f, want) {
				t.Errorf("the build's flags %q lack %q", f, want)
			}
		}
		if strings.Contains(f, "version.Repo") || strings.Contains(f, "ReleaseKeys") {
			t.Errorf("a development build was stamped as a release: %q", f)
		}
	}
	for target, member := range map[string]string{"linux/amd64": "tenant", "windows/arm64": "tenant.exe"} {
		goos, goarch, _ := strings.Cut(target, "/")
		archive := filepath.Join(dest, selfupdate.AssetName(v, goos, goarch))
		if gv, gos, garch, ok := selfupdate.ParseAssetName(filepath.Base(archive)); !ok || gv != v || gos != goos || garch != goarch {
			t.Errorf("%s reads back as %q %q %q %v", filepath.Base(archive), gv, gos, garch, ok)
		}
		bin := filepath.Join(t.TempDir(), member)
		if err := selfupdate.ExtractBinary(archive, bin, member); err != nil {
			t.Errorf("%s: %v", target, err)
			continue
		}
		if b, _ := os.ReadFile(bin); string(b) != "a tenant for "+target {
			t.Errorf("%s holds %q", filepath.Base(archive), b)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dest, "*.tmp")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	// A tree with changes that were not committed says so in the version.
	status = " M cmd/tenant/main.go"
	out.Reset()
	if err := run([]string{"-o", dest, "linux/arm64"}, &out); err != nil || !strings.Contains(out.String(), "Tenant "+v+".dirty") {
		t.Errorf("a dirty tree: %v\n%s", err, out.String())
	}
	// all is the six targets of a release; a target no release has is refused
	// before anything is built.
	status, flags = "", nil
	if err := run([]string{"-o", dest, "all"}, &out); err != nil || len(flags) != len(releaseTargets) {
		t.Errorf("all: %v, %d builds", err, len(flags))
	}
	flags = nil
	for _, bad := range [][]string{{"plan9/amd64"}, {"linux"}, {"linux/amd64", "linux/amd64"}, {"all", "linux/amd64"}} {
		if err := run(append([]string{"-o", dest}, bad...), &out); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(flags) != 0 {
		t.Errorf("a refused target was built: %d builds", len(flags))
	}
}
