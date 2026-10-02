// Package selfupdate is the machinery under `tenant update` (TEN-500): find a
// release on GitHub, fetch its archive and checksums, verify them, take the
// binary out, and swap it into place with no instant at which the installed
// `tenant` is missing. It is stdlib only and a leaf: it knows nothing about
// the command, the service manager or the data directory, so cmd/tenant
// sequences these steps (back up, confirm, restart, health-check) and serve's
// daily check shares Latest without pulling anything else in.
package selfupdate

import (
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
