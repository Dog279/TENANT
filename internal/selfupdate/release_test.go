package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAssetName(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "tenant_0.3.0_linux_x86_64.tar.gz"},
		{"linux", "arm64", "tenant_0.3.0_linux_aarch64.tar.gz"},
		{"darwin", "amd64", "tenant_0.3.0_darwin_x86_64.tar.gz"},
		{"darwin", "arm64", "tenant_0.3.0_darwin_aarch64.tar.gz"},
		{"windows", "amd64", "tenant_0.3.0_windows_x86_64.zip"},
		{"windows", "arm64", "tenant_0.3.0_windows_aarch64.zip"},
	} {
		if got := AssetName("0.3.0", tc.goos, tc.goarch); got != tc.want {
			t.Errorf("AssetName(0.3.0, %s, %s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// ParseAssetName reads back what AssetName wrote, a development build's long
// version included, and nothing else: the name is all a build handed to
// `tenant fleet update --build` says of itself.
func TestParseAssetName(t *testing.T) {
	for _, v := range []string{"0.3.0", "0.3.0-rc.2", "0.3.0-dev.20261126193000.e56d774.dirty"} {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			for _, goarch := range []string{"amd64", "arm64"} {
				name := AssetName(v, goos, goarch)
				gv, gos, garch, ok := ParseAssetName(name)
				if !ok || gv != v || gos != goos || garch != goarch {
					t.Errorf("ParseAssetName(%q) = %q, %q, %q, %v", name, gv, gos, garch, ok)
				}
			}
		}
	}
	for _, name := range []string{
		"", "tenant", "tenant.exe", "tenant_0.3.0_linux_x86_64", "tenant_0.3.0_linux_x86_64.tgz",
		"tenant_0.3.0_linux_x86_64.zip",      // a zip is Windows's
		"tenant_0.3.0_windows_x86_64.tar.gz", // and Windows's is a zip
		"tenant_0.3.0_linux_riscv64.tar.gz",  // no release is built for it
		"tenant__linux_x86_64.tar.gz", "tenant_0.3.0__x86_64.tar.gz", "tenant_linux_x86_64.tar.gz",
		"other_0.3.0_linux_x86_64.tar.gz", "checksums.txt",
	} {
		if v, goos, goarch, ok := ParseAssetName(name); ok {
			t.Errorf("ParseAssetName(%q) = %q, %q, %q: accepted", name, v, goos, goarch)
		}
	}
}

func TestVersionOf(t *testing.T) {
	for tag, want := range map[string]string{"v0.3.0": "0.3.0", "0.3.0": "0.3.0", "v1.0.0-rc.1": "1.0.0-rc.1", "vv1": "v1"} {
		if got := VersionOf(tag); got != want {
			t.Errorf("VersionOf(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestLatestAndByTag(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v1.2.3","assets":[`+
				`{"name":"checksums.txt","url":"https://api/assets/1","browser_download_url":"https://web/1"},`+
				`{"name":"tenant_1.2.3_linux_x86_64.tar.gz","url":"https://api/assets/2","browser_download_url":"https://web/2"}]}`)
		case "/repos/o/r/releases/tags/v1.0.0":
			fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[]}`)
		case "/repos/limited/r/releases/latest":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		case "/repos/broken/r/releases/latest":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	rel, err := Latest(ctx, srv.Client(), srv.URL, "o/r", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.2.3" || len(rel.Assets) != 2 ||
		rel.Assets["checksums.txt"] != "https://api/assets/1" ||
		rel.Assets["tenant_1.2.3_linux_x86_64.tar.gz"] != "https://api/assets/2" {
		t.Fatalf("Latest = %+v; want the tag and the two API asset URLs", rel)
	}
	if got := seen.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q", got)
	}
	if got := seen.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q, want the bearer", got)
	}

	rel, err = ByTag(ctx, srv.Client(), srv.URL, "o/r", "v1.0.0", "")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.0.0" || len(rel.Assets) != 0 {
		t.Fatalf("ByTag = %+v", rel)
	}
	if _, ok := seen["Authorization"]; ok {
		t.Error("no token, yet an Authorization header was sent")
	}

	for _, tc := range []struct {
		name string
		call func() error
		want string
	}{
		{"missing tag", func() error { _, err := ByTag(ctx, srv.Client(), srv.URL, "o/r", "v9.9.9", ""); return err }, "no such release"},
		{"rate limited", func() error { _, err := Latest(ctx, srv.Client(), srv.URL, "limited/r", ""); return err }, "rate limit"},
		{"other status", func() error { _, err := Latest(ctx, srv.Client(), srv.URL, "broken/r", ""); return err }, "502"},
	} {
		err := tc.call()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("tenant!"), 100_000)
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		if r.URL.Path != "/asset" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.tar.gz")

	if err := Download(ctx, srv.Client(), srv.URL+"/asset", "tok", dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(payload))
	}
	if got := seen.Get("Accept"); got != "application/octet-stream" {
		t.Errorf("Accept = %q", got)
	}
	if got := seen.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}

	err := Download(ctx, srv.Client(), srv.URL+"/missing", "", filepath.Join(dir, "b.tar.gz"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: err = %v", err)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "a.tar.gz" {
		t.Fatalf("dir holds %v; want only a.tar.gz: no partial file, no temp leftovers", names)
	}
}

// dirNames is what a directory holds, so a test can prove temp files are
// renamed away, never left behind.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// Compare orders versions by their numbers, then a pre-release before its
// release and one pre-release from the next (a release candidate from the
// one after it, a development build from both), and refuses what is not a
// version (a fleet machine's reported version is its own word, and anything
// may be in it).
func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		c    int
		ok   bool
	}{
		{"1.2.3", "1.2.3", 0, true},
		{"v1.2.3", "1.2.3", 0, true},
		{"1.2.3", "1.2.10", -1, true},
		{"1.10.0", "1.9.9", 1, true},
		{"2.0", "2.0.0", 0, true},
		{"1.2.0-rc1", "1.2.0", -1, true},
		{"1.2.0", "1.2.0-rc1", 1, true},
		{"0.3.0-rc.1", "0.3.0-rc.2", -1, true},
		{"0.3.0-rc.10", "0.3.0-rc.2", 1, true},
		{"v0.3.0-rc.2", "0.3.0-rc.2", 0, true},
		{"0.3.0-rc.2", "0.3.1-rc.1", -1, true},
		{"0.3.0-rc.2+build.5", "0.3.0-rc.2", 0, true},
		{"0.3.0-dev.20261126193000.e56d774", "0.3.0-rc.1", -1, true},
		{"0.3.0-dev.20261126193000.e56d774", "0.3.0-dev.20261127080000.0a1b2c3", -1, true},
		{"0.3.0-dev.20261126193000.e56d774.dirty", "0.3.0-dev.20261126193000.e56d774", 1, true},
		{"0.3.0-dev.20261126193000.e56d774", "0.3.0", -1, true},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1, true},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1, true},
		{"1.0.0-1", "1.0.0-rc", -1, true},
		{"1.0.0-", "1.0.0", 0, true},
		{"1.0.0-99999999999999999999", "1.0.0-rc", -1, true}, // too long for a number: a word, and "9" sorts before "r"
		{"1.2.0+build.5", "1.2.1", -1, true},
		{"0.0.1", "0.3.0", -1, true},
		{"", "1.0.0", 0, false},
		{"1.x.0", "1.0.0", 0, false},
		{"1..2", "1.0.0", 0, false},
		{"1.2.3.4.5", "1.0.0", 0, false},
		{"-1.0", "1.0.0", 0, false},
		{"1234567890.0", "1.0.0", 0, false},
		{`"1.2.3"`, "1.2.3", 0, false},
	} {
		if c, ok := Compare(tc.a, tc.b); c != tc.c || ok != tc.ok {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, c, ok, tc.c, tc.ok)
		}
	}
}
