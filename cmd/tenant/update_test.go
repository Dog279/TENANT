package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event/eventtest"
	"tenant/internal/selfupdate"
	"tenant/internal/version"
)

// buildFakeBin builds testdata/fakebin for this OS: the `tenant` a release
// archive carries, answering `version` from TENANT_FAKE_VERSION.
func buildFakeBin(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a helper binary")
	}
	out := filepath.Join(t.TempDir(), exeName())
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakebin")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakebin: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// releaseArchive packs bin the way GoReleaser does for this OS: a flat
// tar.gz, or a zip on Windows.
func releaseArchive(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var closers []func() error
	var w io.Writer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		f, err := zw.Create(exeName())
		if err != nil {
			t.Fatal(err)
		}
		w, closers = f, []func() error{zw.Close}
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: exeName(), Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		w, closers = tw, []func() error{tw.Close, gz.Close}
	}
	if _, err := w.Write(bin); err != nil {
		t.Fatal(err)
	}
	for _, c := range closers {
		if err := c(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// fakeRelease is one httptest server playing GitHub (the release API and
// asset downloads for repo o/r) and the hub (/api/status).
type fakeRelease struct {
	srv     *httptest.Server
	archive []byte
	sums    string
	assets  map[string][]byte // more assets of the release, by name (set before the run)

	mu        sync.Mutex
	hubVersion string   // what /api/status reports
	badSums    bool     // serve a checksums.txt that does not match
	auth       []string // Authorization headers the release API saw
	paths      []string // every request's path, in order
}

func newFakeRelease(t *testing.T, tag string, archive []byte) *fakeRelease {
	t.Helper()
	asset := selfupdate.AssetName(selfupdate.VersionOf(tag), runtime.GOOS, runtime.GOARCH)
	f := &fakeRelease{archive: archive, sums: fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset), assets: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			f.auth = append(f.auth, r.Header.Get("Authorization"))
		}
		hub, bad := f.hubVersion, f.badSums
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/repos/o/r/releases/latest" || r.URL.Path == "/repos/o/r/releases/tags/"+tag:
			list := []map[string]string{
				{"name": asset, "url": f.srv.URL + "/assets/" + asset},
				{"name": "checksums.txt", "url": f.srv.URL + "/assets/checksums.txt"},
			}
			for name := range f.assets {
				list = append(list, map[string]string{"name": name, "url": f.srv.URL + "/assets/" + name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": list})
		case f.assets[strings.TrimPrefix(r.URL.Path, "/assets/")] != nil:
			_, _ = w.Write(f.assets[strings.TrimPrefix(r.URL.Path, "/assets/")])
		case r.URL.Path == "/assets/checksums.txt" || r.URL.Path == "/assets/"+asset:
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "assets are served as octet-stream", http.StatusNotAcceptable)
				return
			}
			if r.URL.Path == "/assets/"+asset {
				_, _ = w.Write(archive)
				return
			}
			sums := f.sums
			if bad {
				sums = "0000" + sums[4:]
			}
			_, _ = io.WriteString(w, sums)
		case r.URL.Path == "/api/status":
			fmt.Fprintf(w, `{"version":%q}`, hub)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// releaseBuild makes this test binary look like release v of o/r until the
// test ends, with no dogfooding environment leaking in.
func releaseBuild(t *testing.T, v string) {
	t.Helper()
	pv, pd, pr := version.Version, version.Dirty, version.Repo
	version.Version, version.Dirty, version.Repo = v, "false", "o/r"
	t.Cleanup(func() { version.Version, version.Dirty, version.Repo = pv, pd, pr })
	t.Setenv("TENANT_UPDATE_REPO", "")
	t.Setenv("GITHUB_TOKEN", "")
}

// install is a fake install: an old binary, a config dir, a data dir with
// something in it, and a recorder in place of the service restart.
type install struct {
	exe, cfg, data string
	restarts       []bool // the now flag of each restart
	out            bytes.Buffer
}

func newInstall(t *testing.T) *install {
	t.Helper()
	root := t.TempDir()
	in := &install{exe: filepath.Join(root, "bin", exeName()), cfg: filepath.Join(root, "cfg"), data: filepath.Join(root, "data")}
	for _, d := range []string{filepath.Dir(in.exe), in.cfg, in.data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(in.exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.data, "note.txt"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	return in
}

func (in *install) env(f *fakeRelease, stdin string) *updateEnv {
	return &updateEnv{c: &commonFlags{cfgDir: in.cfg, dataDir: in.data}, exe: in.exe, apiBase: f.srv.URL, hub: f.srv.URL,
		repo: "o/r", client: f.srv.Client(), stdin: strings.NewReader(stdin), out: &in.out,
		restart: func(_ context.Context, now bool) error { in.restarts = append(in.restarts, now); return nil }}
}

func exitCodeOf(err error) int {
	var ex exitError
	switch {
	case errors.As(err, &ex):
		return ex.code
	case err != nil:
		return 1
	}
	return 0
}

// The flow against a fake release and hub: what each outcome leaves on
// disk, prints, and asks of the service.
func TestUpdateFlow(t *testing.T) {
	bin := buildFakeBin(t)
	archive := releaseArchive(t, bin)
	t.Setenv("TENANT_FAKE_VERSION", "9.9.9")
	prevPoll, prevHealth := updatePoll, updateHealth
	updatePoll, updateHealth = 5*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { updatePoll, updateHealth = prevPoll, prevHealth })

	for _, tc := range []struct {
		name      string
		running   string // version.Version
		hub       string // what /api/status reports
		opts      updateOpts
		stdin     string
		noService bool
		prep      func(t *testing.T, f *fakeRelease)
		code      int
		out       string // in stdout
		errText   string // in the error
		swapped   bool   // the exe is the new binary
		prev      bool   // tenant.prev holds the old one
		backup    bool   // the pre-update backup was written
		restarts  []bool
		path      string // a request path the release API must have seen
		auth      string // an Authorization header it must have seen
	}{
		{name: "happy path", running: "1.0.0", hub: "9.9.9", opts: updateOpts{yes: true},
			out: "updated 1.0.0 -> 9.9.9", swapped: true, prev: true, backup: true, restarts: []bool{false}},
		{name: "confirmed at the prompt", running: "1.0.0", hub: "9.9.9", stdin: "y\n",
			out: "Update 1.0.0 -> 9.9.9? A backup goes to", swapped: true, prev: true, backup: true, restarts: []bool{false}},
		{name: "declined at the prompt", running: "1.0.0", hub: "9.9.9", stdin: "\n", code: 2, errText: "not confirmed"},
		{name: "no service: the operator restarts", running: "1.0.0", opts: updateOpts{yes: true, now: true}, noService: true,
			out: "restart Tenant to finish the update", swapped: true, prev: true, backup: true},
		{name: "checksum mismatch", running: "1.0.0", hub: "9.9.9", opts: updateOpts{yes: true},
			prep: func(_ *testing.T, f *fakeRelease) { f.badSums = true }, code: 1, errText: "sha256 mismatch"},
		{name: "smoke failure", running: "1.0.0", hub: "9.9.9", opts: updateOpts{yes: true},
			prep: func(t *testing.T, _ *fakeRelease) { t.Setenv("TENANT_FAKE_FAIL", "1") }, code: 1, errText: "smoke test"},
		{name: "health mismatch rolls back", running: "1.0.0", hub: "1.0.0", opts: updateOpts{yes: true},
			code: 1, errText: "rolled the binary back; if the new version migrated data, restore with: tenant service stop && tenant backup restore",
			backup: true, restarts: []bool{false, true}},
		{name: "dev build refused", running: "0.0.1", opts: updateOpts{yes: true}, code: 2, errText: "development build"},
		{name: "dev build with --force", running: "0.0.1", hub: "9.9.9", opts: updateOpts{yes: true, force: true},
			out: "updated 0.0.1 -> 9.9.9", swapped: true, prev: true, backup: true, restarts: []bool{false}},
		{name: "--check: update available", running: "1.0.0", opts: updateOpts{check: true}, code: 10, out: "update available: 1.0.0 -> 9.9.9"},
		{name: "--check: up to date", running: "9.9.9", opts: updateOpts{check: true}, out: "up to date (9.9.9)"},
		{name: "--version pins a tag", running: "1.0.0", opts: updateOpts{check: true, version: "v9.9.9"}, code: 10, path: "/repos/o/r/releases/tags/v9.9.9"},
		{name: "GITHUB_TOKEN is the bearer", running: "1.0.0", opts: updateOpts{check: true},
			prep: func(t *testing.T, _ *fakeRelease) { t.Setenv("GITHUB_TOKEN", "tok") }, code: 10, auth: "Bearer tok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releaseBuild(t, tc.running)
			f := newFakeRelease(t, "v9.9.9", archive)
			f.hubVersion = tc.hub
			if tc.prep != nil {
				tc.prep(t, f)
			}
			in := newInstall(t)
			u := in.env(f, tc.stdin)
			if tc.noService {
				u.restart = nil
			}
			err := runUpdate(context.Background(), u, tc.opts)
			if exitCodeOf(err) != tc.code {
				t.Fatalf("exit %d (%v), want %d\n%s", exitCodeOf(err), err, tc.code, in.out.String())
			}
			if tc.errText != "" && (err == nil || !strings.Contains(err.Error(), tc.errText)) {
				t.Errorf("err = %v, want it to say %q", err, tc.errText)
			}
			if !strings.Contains(in.out.String(), tc.out) {
				t.Errorf("stdout:\n%s\nwant %q", in.out.String(), tc.out)
			}
			got, err := os.ReadFile(in.exe)
			if err != nil {
				t.Fatalf("the binary must exist at every outcome: %v", err)
			}
			if swapped := bytes.Equal(got, bin); swapped != tc.swapped {
				t.Errorf("binary swapped = %v, want %v", swapped, tc.swapped)
			}
			if prev, err := os.ReadFile(selfupdate.PrevPath(in.exe)); (err == nil) != tc.prev || tc.prev && string(prev) != "old binary" {
				t.Errorf(".prev: %q, %v; want present=%v holding the old binary", prev, err, tc.prev)
			}
			if _, err := os.Stat(preUpdateBackupPath(in.data, tc.running)); (err == nil) != tc.backup {
				t.Errorf("backup written = %v, want %v", err == nil, tc.backup)
			}
			if _, ok, _ := selfupdate.ReadState(updateMarker(in.data)); ok {
				t.Error("the marker was left behind")
			}
			if !slices.Equal(in.restarts, tc.restarts) {
				t.Errorf("restarts (now flags) = %v, want %v", in.restarts, tc.restarts)
			}
			if tc.path != "" && !slices.Contains(f.paths, tc.path) {
				t.Errorf("requests %v, want %s among them", f.paths, tc.path)
			}
			if tc.auth != "" && !slices.Contains(f.auth, tc.auth) {
				t.Errorf("authorization headers %q, want %q among them", f.auth, tc.auth)
			}
		})
	}
}

// A marker left by an interrupted update: finished when the new version is
// what runs, offered a rollback otherwise; serve files it at boot; doctor
// warns; --rollback puts the previous binary back.
func TestUpdateInterruptedMarker(t *testing.T) {
	releaseBuild(t, "9.9.9")
	f := newFakeRelease(t, "v9.9.9", nil)
	in := newInstall(t)
	ctx := context.Background()
	marker := updateMarker(in.data)
	write := func(to string) {
		t.Helper()
		if err := selfupdate.WriteState(marker, selfupdate.State{From: "1.0.0", To: to, Step: "swap", Started: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	write("9.9.9")
	if err := runUpdate(ctx, in.env(f, ""), updateOpts{check: true}); err != nil {
		t.Fatalf("finished update: %v", err)
	}
	if _, ok, _ := selfupdate.ReadState(marker); ok || !strings.Contains(in.out.String(), "update to 9.9.9 finished") {
		t.Errorf("marker kept = %v, stdout:\n%s", ok, in.out.String())
	}

	write("8.8.8")
	err := runUpdate(ctx, in.env(f, ""), updateOpts{check: true})
	if exitCodeOf(err) != 2 || !strings.Contains(err.Error(), "--rollback") {
		t.Errorf("another version's marker: exit %d, %v; want 2 and the rollback offer", exitCodeOf(err), err)
	}
	if _, ok, _ := selfupdate.ReadState(marker); !ok {
		t.Error("the marker must stay until the operator decides")
	}

	rec := eventtest.Capture(t)
	reportInterruptedUpdate(ctx, in.data)
	reportInterruptedUpdate(ctx, filepath.Join(in.data, "none")) // no marker: nothing
	want := "The update from 1.0.0 to 8.8.8 was interrupted at swap; running 9.9.9"
	if ev := rec.Events(); len(ev) != 1 || ev[0].ID != catalog.UpdateInterrupted.Spec().ID || ev[0].Msg != want {
		t.Errorf("boot events = %v, want one UpdateInterrupted %q", ev, want)
	}

	if r := checkUpdate(ctx, &doctorEnv{c: &commonFlags{dataDir: in.data}}); r.Status != statusWarn || !strings.Contains(r.Detail, "interrupted at swap; running 9.9.9") {
		t.Errorf("doctor = %+v, want WARN with the marker's text", r)
	}

	if err := os.WriteFile(selfupdate.PrevPath(in.exe), []byte("previous"), 0o755); err != nil {
		t.Fatal(err)
	}
	in.out.Reset()
	if err := runUpdate(ctx, in.env(f, ""), updateOpts{rollback: true}); err != nil {
		t.Fatalf("--rollback: %v", err)
	}
	if b, _ := os.ReadFile(in.exe); string(b) != "previous" {
		t.Errorf("binary after rollback = %q", b)
	}
	if _, ok, _ := selfupdate.ReadState(marker); ok || !slices.Equal(in.restarts, []bool{false}) || !strings.Contains(in.out.String(), "tenant backup restore") {
		t.Errorf("after rollback: marker kept = %v, restarts %v, stdout:\n%s", ok, in.restarts, in.out.String())
	}
}

// serve's daily check: one UpdateAvailable per new tag, none for the running
// version, no request when opted out, nothing on a network error.
func TestServeUpdateCheck(t *testing.T) {
	releaseBuild(t, "1.0.0")
	f := newFakeRelease(t, "v9.9.9", nil)
	rec := eventtest.Capture(t)
	ctx := context.Background()
	available := func() int {
		n := 0
		for _, e := range rec.Events() {
			if e.Provider == "system" && e.ID == catalog.UpdateAvailable.Spec().ID {
				n++
			}
		}
		return n
	}

	announced := ""
	updateCheck(ctx, nil, f.srv.Client(), f.srv.URL, &announced)
	updateCheck(ctx, nil, f.srv.Client(), f.srv.URL, &announced) // the same tag the next day
	if n := available(); n != 1 || announced != "v9.9.9" {
		t.Errorf("a newer tag, twice: %d events, announced %q", n, announced)
	}

	version.Version, announced = "9.9.9", ""
	updateCheck(ctx, nil, f.srv.Client(), f.srv.URL, &announced)
	if n := available(); n != 1 {
		t.Errorf("the running version: %d events, want still 1", n)
	}

	version.Version = "1.0.0"
	off := false
	before := len(f.paths)
	updateCheck(ctx, &launchConfig{UpdateCheck: &off}, f.srv.Client(), f.srv.URL, &announced)
	if len(f.paths) != before || available() != 1 {
		t.Errorf("opted out: %d requests, %d events", len(f.paths)-before, available())
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	updateCheck(ctx, nil, gone.Client(), gone.URL, &announced)
	if n := available(); n != 1 {
		t.Errorf("a connection error: %d events, want still 1", n)
	}
}

// TestUpdateLive resolves, downloads, verifies, extracts and runs the real
// latest release for this host. No swap.
func TestUpdateLive(t *testing.T) {
	repo, token := os.Getenv("TENANT_UPDATE_REPO"), os.Getenv("GITHUB_TOKEN")
	if os.Getenv("TENANT_LIVE_UPDATE") != "1" || repo == "" || token == "" {
		t.Skip("TENANT_LIVE_UPDATE=1 with GITHUB_TOKEN and TENANT_UPDATE_REPO runs this against the real releases")
	}
	ctx := context.Background()
	rel, err := selfupdate.Latest(ctx, nil, "", repo, token)
	if err != nil {
		t.Fatal(err)
	}
	v := selfupdate.VersionOf(rel.Tag)
	dir := t.TempDir()
	asset := selfupdate.AssetName(v, runtime.GOOS, runtime.GOARCH)
	for _, name := range []string{asset, "checksums.txt"} {
		url, ok := rel.Assets[name]
		if !ok {
			t.Fatalf("release %s has no %s", rel.Tag, name)
		}
		if err := selfupdate.Download(ctx, nil, url, token, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := selfupdate.Verify(filepath.Join(dir, asset), filepath.Join(dir, "checksums.txt")); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, exeName())
	if err := selfupdate.ExtractBinary(filepath.Join(dir, asset), bin, exeName()); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), v) {
		t.Fatalf("%s version: %v\n%s", rel.Tag, err, out)
	}
}
