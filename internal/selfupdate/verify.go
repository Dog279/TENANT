package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ParseChecksums reads GoReleaser's checksums.txt (sha256sum's
// `<hex>  <name>` lines) into name → lowercase hex. It tolerates the binary
// marker (`*name`), blank lines and names containing spaces; a line it cannot
// read is skipped and then simply matches nothing.
func ParseChecksums(r io.Reader) map[string]string {
	sums := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		sum, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		name = strings.TrimPrefix(strings.TrimLeft(name, " "), "*")
		if !ok || name == "" {
			continue
		}
		sums[name] = strings.ToLower(sum)
	}
	return sums
}

// SHA256 is the lowercase hex digest of the file at path.
func SHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Verify checks archivePath against the entry for its base name in
// checksumsPath. A missing entry fails too: an archive the release does not
// list is not one of ours.
func Verify(archivePath, checksumsPath string) error {
	f, err := os.Open(checksumsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	name, list := filepath.Base(archivePath), filepath.Base(checksumsPath)
	want, ok := ParseChecksums(f)[name]
	if !ok {
		return fmt.Errorf("%s lists no checksum for %s", list, name)
	}
	got, err := SHA256(archivePath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: sha256 mismatch: %s lists %s, the file hashes to %s", name, list, want, got)
	}
	return nil
}

// CosignVerify checks the Sigstore bundle the release workflow signed
// checksums.txt with, through the cosign on PATH, run via the caller's
// command seam. The certificate must name this repository's release
// workflow and GitHub's OIDC issuer; any other signer, and any error at all,
// fails closed. The repository is quoted so a "." in its name cannot be
// matched by a look-alike repository's workflow.
//
// ponytail: cosign on PATH, not in-process verification, which needs
// sigstore-go (a new module); revisit when the operator base outgrows
// "cosign installed where it matters".
func CosignVerify(ctx context.Context, run func(ctx context.Context, name string, args ...string) (string, error), checksumsPath, bundlePath, repo string) error {
	_, err := run(ctx, "cosign", CosignArgs(checksumsPath, bundlePath, repo, false)...)
	if err != nil {
		return fmt.Errorf("cosign verify-blob %s: %w", filepath.Base(checksumsPath), err)
	}
	return nil
}

// CosignArgs are cosign's arguments for verifying the bundle over
// checksums.txt as signed by repo's release workflow. offline adds
// --offline: the bundle's own log entry and cosign's trusted root, no
// network (a fleet child, which has none, decision A71). An empty repo is
// refused by the caller: it would match any repository.
func CosignArgs(checksumsPath, bundlePath, repo string, offline bool) []string {
	args := []string{"verify-blob",
		"--bundle", bundlePath,
		"--certificate-identity-regexp", "^https://github.com/" + regexp.QuoteMeta(repo) + `/\.github/workflows/release\.yml@`,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com"}
	if offline {
		args = append(args, "--offline")
	}
	return append(args, checksumsPath)
}

// HaveCosign reports whether cosign is on PATH. Without it an update is
// checksum-only, which trusts GitHub's TLS for authenticity, and the command
// says so.
func HaveCosign() bool {
	_, err := exec.LookPath("cosign")
	return err == nil
}
