package selfupdate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseChecksums(t *testing.T) {
	in := "abc123  tenant_1.0_linux_x86_64.tar.gz\n" +
		"\n" +
		"DEF456 *tenant_1.0_windows_x86_64.zip\n" +
		"0011  name with spaces.txt\n" +
		"malformed\n" +
		"  ffff  indented.txt  \n"
	want := map[string]string{
		"tenant_1.0_linux_x86_64.tar.gz": "abc123",
		"tenant_1.0_windows_x86_64.zip":  "def456",
		"name with spaces.txt":           "0011",
		"indented.txt":                   "ffff",
	}
	if got := ParseChecksums(strings.NewReader(in)); !maps.Equal(got, want) {
		t.Fatalf("ParseChecksums = %v, want %v", got, want)
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "tenant_1.0_linux_x86_64.tar.gz")
	if err := os.WriteFile(archive, []byte("not really an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("not really an archive")))
	if got, err := SHA256(archive); err != nil || got != sum {
		t.Fatalf("SHA256 = %q, %v; want %q", got, err, sum)
	}
	for _, tc := range []struct{ name, lines, want string }{
		{"match", sum + "  tenant_1.0_linux_x86_64.tar.gz\n", ""},
		{"mismatch", strings.Repeat("0", 64) + "  tenant_1.0_linux_x86_64.tar.gz\n", "mismatch"},
		{"missing entry", sum + "  tenant_1.0_darwin_x86_64.tar.gz\n", "no checksum for tenant_1.0_linux_x86_64.tar.gz"},
	} {
		sums := filepath.Join(dir, "checksums.txt")
		if err := os.WriteFile(sums, []byte(tc.lines), 0o600); err != nil {
			t.Fatal(err)
		}
		err := Verify(archive, sums)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: Verify = %v, want nil", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: Verify = %v, want an error mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestCosignVerifyArgv(t *testing.T) {
	var got []string
	fail := errors.New("exit status 1")
	var runErr error
	run := func(_ context.Context, name string, args ...string) (string, error) {
		got = append([]string{name}, args...)
		return "", runErr
	}
	ctx := context.Background()
	if err := CosignVerify(ctx, run, "/stage/checksums.txt", "/stage/checksums.txt.sigstore.json", "o/r"); err != nil {
		t.Fatal(err)
	}
	want := []string{"cosign", "verify-blob",
		"--bundle", "/stage/checksums.txt.sigstore.json",
		"--certificate-identity-regexp", `^https://github.com/o/r/\.github/workflows/release\.yml@`,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
		"/stage/checksums.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant   %q", got, want)
	}
	runErr = fail
	if err := CosignVerify(ctx, run, "/stage/checksums.txt", "/stage/b.json", "o/r"); !errors.Is(err, fail) {
		t.Fatalf("a failing cosign must fail closed; err = %v", err)
	}
}
