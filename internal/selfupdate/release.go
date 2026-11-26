// Package selfupdate is the machinery under `tenant update` (TEN-500): find a
// release on GitHub, fetch its archive and checksums, verify them, take the
// binary out, and swap it into place with no instant at which the installed
// `tenant` is missing. It is stdlib only and a leaf: it knows nothing about
// the command, the service manager or the data directory, so cmd/tenant
// sequences these steps (back up, confirm, restart, health-check) and serve's
// daily check shares Latest without pulling anything else in.
package selfupdate

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Release is one GitHub release: its tag and, per asset name, the asset's API
// URL (the `url` field, not browser_download_url). Fetched with
// Accept: application/octet-stream the API URL serves public and private
// repositories alike, which browser_download_url does not.
type Release struct {
	Tag    string
	Assets map[string]string
}

// defaultAPIBase is used when apiBase is empty; tests pass an httptest URL.
const defaultAPIBase = "https://api.github.com"

// Latest resolves repo's latest release. GitHub's "latest" skips prereleases
// and drafts, so a release candidate is reached only through ByTag.
func Latest(ctx context.Context, client *http.Client, apiBase, repo, token string) (Release, error) {
	return getRelease(ctx, client, apiBase, "/repos/"+repo+"/releases/latest", token)
}

// ByTag resolves the release tagged tag, "v" included.
func ByTag(ctx context.Context, client *http.Client, apiBase, repo, tag, token string) (Release, error) {
	return getRelease(ctx, client, apiBase, "/repos/"+repo+"/releases/tags/"+tag, token)
}

func getRelease(ctx context.Context, client *http.Client, apiBase, path, token string) (Release, error) {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	resp, err := get(ctx, client, apiBase+path, token, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	limited := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// A private repository answers 404, not 401, to a request without a token.
		return Release{}, fmt.Errorf("github: %s: no such release (a private repository needs a token)", path)
	case limited && resp.Header.Get("X-RateLimit-Remaining") == "0":
		when := "soon"
		if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			when = "in " + time.Until(time.Unix(n, 0)).Round(time.Minute).String()
		}
		return Release{}, fmt.Errorf("github: rate limit exhausted, it resets %s (a token raises the limit)", when)
	case resp.StatusCode != http.StatusOK:
		return Release{}, fmt.Errorf("github: GET %s: %s", path, resp.Status)
	}
	var body struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	// A release document is kilobytes; the bound keeps a wrong endpoint from
	// being read forever.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("github: GET %s: %w", path, err)
	}
	r := Release{Tag: body.Tag, Assets: make(map[string]string, len(body.Assets))}
	for _, a := range body.Assets {
		r.Assets[a.Name] = a.URL
	}
	return r, nil
}

// get issues one GET with GitHub's headers; a nil client is the default one.
func get(ctx context.Context, client *http.Client, url, token, accept string) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}

// VersionOf is a tag without its leading "v": "v0.3.0" is version "0.3.0",
// the form version.Version holds.
func VersionOf(tag string) string { return strings.TrimPrefix(tag, "v") }

// Compare orders two versions ("1.2.3", a leading "v" allowed) the way
// semantic versioning does: -1, 0 or 1. Their dotted numbers decide first.
// With the same numbers, a pre-release ("1.2.0-rc.1") comes before the
// release it leads to ("1.2.0"), and two pre-releases go by their parts
// between the dots: a number before a word, numbers by value, words in ASCII
// order, and the shorter first when it is the start of the longer. So
// 0.3.0-dev.20261126 < 0.3.0-rc.1 < 0.3.0-rc.2 < 0.3.0: a release candidate
// is told from the next one, and a development build (scripts/devbuild) from
// both. Build metadata after "+" is ignored. ok is false when either is not
// a version of that form.
func Compare(a, b string) (c int, ok bool) {
	pa, prea, oka := numbers(a)
	pb, preb, okb := numbers(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return cmp.Compare(x, y), true
		}
	}
	return comparePre(prea, preb), true
}

// comparePre orders what follows the "-" of two versions whose numbers are
// equal. No pre-release at all is the release itself, the later of the two.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < min(len(as), len(bs)); i++ {
		x, xnum := preNumber(as[i])
		y, ynum := preNumber(bs[i])
		switch {
		case xnum && ynum:
			if x != y {
				return cmp.Compare(x, y)
			}
		case xnum != ynum: // a number comes before a word
			if xnum {
				return -1
			}
			return 1
		case as[i] != bs[i]:
			return strings.Compare(as[i], bs[i])
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// preNumber reads one part of a pre-release as a number: digits only, and
// few enough to hold (a timestamp is 14).
func preNumber(s string) (int64, bool) {
	if len(s) == 0 || len(s) > 18 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// numbers splits a version into its dotted numbers and its pre-release, the
// text after the first "-" with any build metadata cut off.
func numbers(v string) (nums []int, pre string, ok bool) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, pre, _ = strings.Cut(v, "-")
	var out []int
	for _, f := range strings.Split(v, ".") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || len(f) > 9 {
			return nil, "", false
		}
		out = append(out, n)
	}
	return out, pre, len(out) > 0 && len(out) <= 4
}

// AssetName is the archive GoReleaser publishes for one target, mirroring
// the name_template in .goreleaser.yaml: tenant_<version>_<os>_<arch>.tar.gz,
// .zip on Windows, with amd64 and arm64 spelled x86_64 and aarch64.
func AssetName(version, goos, goarch string) string {
	arch := goarch
	switch goarch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "tenant_" + version + "_" + goos + "_" + arch + ext
}

// ParseAssetName reads an archive's name back: the version, system and
// processor that AssetName makes it from. ok is false for any other name, a
// .zip that is not Windows's and a .tar.gz that is among them. It is how a
// build handed to `tenant fleet update --build` says what it is.
func ParseAssetName(name string) (version, goos, goarch string, ok bool) {
	rest, found := strings.CutPrefix(name, "tenant_")
	if !found {
		return "", "", "", false
	}
	if r, zip := strings.CutSuffix(rest, ".zip"); zip {
		rest = r
	} else if rest, found = strings.CutSuffix(rest, ".tar.gz"); !found {
		return "", "", "", false
	}
	if r, is := strings.CutSuffix(rest, "_x86_64"); is {
		rest, goarch = r, "amd64"
	} else if r, is := strings.CutSuffix(rest, "_aarch64"); is {
		rest, goarch = r, "arm64"
	} else {
		return "", "", "", false
	}
	i := strings.LastIndexByte(rest, '_')
	if i <= 0 || i == len(rest)-1 {
		return "", "", "", false
	}
	version, goos = rest[:i], rest[i+1:]
	if AssetName(version, goos, goarch) != name {
		return "", "", "", false
	}
	return version, goos, goarch, true
}

// Download fetches one release asset by its API URL into dest, streamed
// through a temporary file beside dest, so dest is either absent or complete.
// GitHub answers the API URL with a redirect to a signed storage URL; the
// client follows it and, because the host changes, drops the bearer that
// storage would reject.
func Download(ctx context.Context, client *http.Client, url, token, dest string) error {
	resp, err := get(ctx, client, url, token, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return writeAtomic(dest, 0o600, resp.Body)
}

// writeAtomic streams r into a temporary file beside dest, syncs it and
// renames it over dest: a reader of dest sees the old file or the whole new
// one, never a torn one, and a stop mid-write leaves only a .*.part file.
// The sync makes the bytes durable before the rename makes them visible; the
// binary that becomes `tenant` must never be a zero-length file.
func writeAtomic(dest string, mode os.FileMode, r io.Reader) (err error) {
	f, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.part")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, r); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
