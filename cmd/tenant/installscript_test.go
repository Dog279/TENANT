//go:build !windows

package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/install.sh on macOS (TEN-535): the copy is signed and its flag
// checked before it is moved into place, and a failure leaves what was
// installed before untouched. The script itself runs here, under sh, against
// a release served from a directory, with a uname that says Darwin and a
// codesign that stands in for /usr/bin/codesign: the one line that names
// that path is pointed at the stand-in. The real codesign is not involved;
// the Packaging workflow's macOS job runs the script against it.

const fakeUname = `#!/bin/sh
case "$1" in
-s) echo "$FAKE_OS" ;;
-m) echo "$FAKE_CPU" ;;
esac
`

// The stand-in signs by adding a line to the file, so the signature goes
// where the file goes, and notes what was installed at that moment.
const fakeCodesignScript = `#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$1" in
--force)
  for last; do :; done
  [ "$FAKE_MODE" = fail-sign ] && { echo "$last: the codesign_allocate helper tool cannot be found or used" >&2; exit 1; }
  cat "$FAKE_DEST" > "$FAKE_SEEN" 2>/dev/null
  if [ "$FAKE_MODE" = no-flag ]; then echo "# fake-signed adhoc" >> "$last"; else echo "# fake-signed adhoc,runtime" >> "$last"; fi
  ;;
-dv)
  flags=$(sed -n 's/^# fake-signed //p' "$2")
  [ -n "$flags" ] || { echo "$2: code object is not signed at all" >&2; exit 1; }
  echo "Executable=$2" >&2
  echo "CodeDirectory v=20500 size=1 flags=0x10002($flags) hashes=1+0 location=embedded" >&2
  ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
`

func TestInstallScriptSignsTheCopyBeforeItMoves(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "tar", "sed", "grep", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s here", tool)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(raw), "\r\n", "\n") // a Windows checkout
	const codesignLine = "CODESIGN=/usr/bin/codesign\n"
	if strings.Count(script, codesignLine) != 1 || strings.Count(script, "/usr/bin/codesign") != 1 {
		t.Fatalf("install.sh must name codesign once, by its full path, as %q", strings.TrimSpace(codesignLine))
	}
	const installed = "#!/bin/sh\necho \"tenant 1.2.3\"\n"
	const before = "the Tenant installed before\n"

	for _, tc := range []struct {
		name          string
		os, cpu, arch string
		mode          string // the stand-in codesign's behaviour
		noCodesign    bool
		mvFails       bool
		wantErr       string // in stderr; "" installs
		signs         bool
	}{
		{name: "macOS: signed, checked, moved", os: "Darwin", cpu: "arm64", arch: "aarch64", signs: true},
		{name: "macOS: codesign is missing", os: "Darwin", cpu: "arm64", arch: "aarch64", noCodesign: true, wantErr: "is missing, so the binary cannot be signed"},
		{name: "macOS: signing fails", os: "Darwin", cpu: "arm64", arch: "aarch64", mode: "fail-sign", wantErr: "codesign could not sign the binary"},
		{name: "macOS: the flag is not on the signed copy", os: "Darwin", cpu: "x86_64", arch: "x86_64", mode: "no-flag", wantErr: "the hardened-runtime flag is not on the signed binary"},
		{name: "macOS: the move fails", os: "Darwin", cpu: "arm64", arch: "aarch64", mvFails: true, wantErr: "could not move the binary into place"},
		{name: "Linux: nothing is signed", os: "Linux", cpu: "x86_64", arch: "x86_64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := func(name string) string {
				d := filepath.Join(root, name)
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				return d
			}
			write := func(path, content string, mode os.FileMode) {
				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			bin, release, dest, home := dir("bin"), dir("release"), dir("install"), dir("home")
			write(filepath.Join(bin, "uname"), fakeUname, 0o755)
			codesign := filepath.Join(bin, "codesign")
			if !tc.noCodesign {
				write(codesign, fakeCodesignScript, 0o755)
			}
			if tc.mvFails {
				write(filepath.Join(bin, "mv"), "#!/bin/sh\necho 'mv: cannot move: Read-only file system' >&2\nexit 1\n", 0o755)
			}
			scriptPath := filepath.Join(root, "install.sh")
			write(scriptPath, strings.Replace(script, codesignLine, "CODESIGN="+codesign+"\n", 1), 0o644)

			archive := fmt.Sprintf("tenant_1.2.3_%s_%s.tar.gz", strings.ToLower(tc.os), tc.arch)
			packed := releaseArchive(t, []byte(installed))
			write(filepath.Join(release, archive), string(packed), 0o644)
			write(filepath.Join(release, "checksums.txt"), fmt.Sprintf("%x  %s\n", sha256.Sum256(packed), archive), 0o644)
			target := filepath.Join(dest, "tenant")
			write(target, before, 0o755)

			log, seen := filepath.Join(root, "codesign.log"), filepath.Join(root, "seen")
			cmd := exec.Command("sh", scriptPath)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+home,
				"TENANT_VERSION=1.2.3", "TENANT_BASE_URL=file://"+release, "TENANT_INSTALL_DIR="+dest, "TENANT_REPO=o/r",
				"FAKE_OS="+tc.os, "FAKE_CPU="+tc.cpu, "FAKE_MODE="+tc.mode, "FAKE_LOG="+log, "FAKE_DEST="+target, "FAKE_SEEN="+seen)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			runErr := cmd.Run()
			got, _ := os.ReadFile(target)
			calls, _ := os.ReadFile(log)

			if left, _ := filepath.Glob(filepath.Join(dest, ".tenant.*")); len(left) != 0 {
				t.Errorf("the copy was left behind: %v", left)
			}
			if tc.wantErr != "" {
				var exit *exec.ExitError
				if !errors.As(runErr, &exit) || !strings.Contains(stderr.String(), tc.wantErr) {
					t.Errorf("err = %v, want a failure that says %q\nstderr:\n%s", runErr, tc.wantErr, &stderr)
				}
				if string(got) != before {
					t.Errorf("what was installed before was touched: %q", got)
				}
				return
			}
			if runErr != nil || !strings.Contains(stdout.String(), "Installed "+target) || !strings.Contains(stdout.String(), "tenant 1.2.3") {
				t.Fatalf("install: %v\nstdout:\n%s\nstderr:\n%s", runErr, &stdout, &stderr)
			}
			if !tc.signs {
				if string(got) != installed || len(calls) != 0 {
					t.Errorf("installed %q, codesign was run as:\n%s", got, calls)
				}
				return
			}
			if string(got) != installed+"# fake-signed adhoc,runtime\n" {
				t.Errorf("the installed binary is not the signed copy: %q", got)
			}
			// The copy was signed, with exactly these arguments, then asked
			// for its flags; the binary in place was still the old one.
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			const sign = "--force --sign - --options runtime "
			if len(lines) != 2 || !strings.HasPrefix(lines[0], sign+filepath.Join(dest, ".tenant.")) || lines[1] != "-dv "+strings.TrimPrefix(lines[0], sign) {
				t.Errorf("codesign was run as:\n%s", calls)
			}
			if was, _ := os.ReadFile(seen); string(was) != before {
				t.Errorf("when the copy was signed, the binary in place held %q; want the one installed before", was)
			}
			if !strings.Contains(stdout.String(), "hardened runtime") {
				t.Errorf("the script did not say it signed:\n%s", &stdout)
			}
		})
	}
}
