package sandbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// W1-1: endpoint resolution follows the CLI's precedence and refuses what the
// plan forbids.
func TestResolveEndpoint(t *testing.T) {
	type tc struct {
		name                   string
		explicit, env, context string
		tls                    bool
		goos                   string
		want                   string
		wantErr                string
	}
	for _, c := range []tc{
		{name: "windows default", goos: "windows", want: windowsDesktopPipe},
		{name: "linux default", goos: "linux", want: unixDefaultSocket},
		{name: "context wins over default", goos: "darwin", context: "unix:///Users/u/.docker/run/docker.sock", want: "unix:///Users/u/.docker/run/docker.sock"},
		{name: "DOCKER_HOST wins over context", goos: "linux", env: "unix:///run/user/1000/docker.sock", context: "unix:///var/run/docker.sock", want: "unix:///run/user/1000/docker.sock"},
		{name: "explicit wins over DOCKER_HOST", goos: "linux", explicit: "ssh://box", env: "tcp://evil:2375", want: "ssh://box"},
		{name: "tcp without TLS refused", goos: "linux", env: "tcp://10.0.0.5:2376", wantErr: "without TLS"},
		{name: "tcp with TLS allowed", goos: "linux", env: "tcp://10.0.0.5:2376", tls: true, want: "tcp://10.0.0.5:2376"},
		{name: "plaintext localhost refused even explicit", goos: "windows", explicit: "tcp://127.0.0.1:2375", wantErr: "without TLS"},
		{name: "fd scheme refused", goos: "linux", env: "fd://", wantErr: "only npipe"},
		{name: "not a URL refused", goos: "linux", explicit: "/var/run/docker.sock", wantErr: "not a URL"},
		{name: "whitespace ignored", goos: "windows", env: "   ", want: windowsDesktopPipe},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveEndpoint(c.explicit, c.env, c.context, c.tls, c.goos)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("got (%q, %v), want error containing %q", got, err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got (%q, %v), want %q", got, err, c.want)
			}
		})
	}
}

// W1-1: the CLI's environment carries no DOCKER_* or NVIDIA_*, whatever Tenant
// inherited; everything else stays (docker reads `-e NAME` values from it).
func TestCliEnv(t *testing.T) {
	got := cliEnv([]string{"PATH=/bin", "DOCKER_HOST=tcp://evil:2375", "docker_context=other", "NVIDIA_VISIBLE_DEVICES=all",
		"TENANT_API_KEY=x", "=junk", "NOVALUE", "DOCKERISH=keep"})
	want := []string{"PATH=/bin", "TENANT_API_KEY=x", "DOCKERISH=keep"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("cliEnv = %v, want %v", got, want)
	}
}

func TestLineFloorsCheck(t *testing.T) {
	for _, c := range []struct {
		v      string
		refuse bool
		parsed bool
	}{
		{"1.1.12", true, true}, // older than every fixed line
		{"1.2.7", true, true},  // below its line's floor
		{"1.2.8", false, true}, // the floor itself
		{"1.3.2", true, true},  // below its line's floor
		{"1.3.3", false, true}, // the floor itself
		{"1.4.0-rc.3", false, true},
		{"1.5.1", false, true}, // newer than any listed line: carries the fixes
		{"unknown", false, false},
		{"1", false, false}, // no minor: cannot name a line
	} {
		r, parsed := runcFloors.check("runc", c.v)
		if (r != "") != c.refuse || parsed != c.parsed {
			t.Errorf("runcFloors.check(%q) = (%q, %v), want refuse=%v parsed=%v", c.v, r, parsed, c.refuse, c.parsed)
		}
	}
}

func versionInfo(engine, runc, containerd string) dockerVersionInfo {
	var v dockerVersionInfo
	v.Server.Version = engine
	if runc != "" {
		v.Server.Components = append(v.Server.Components, struct{ Name, Version string }{"runc", runc})
	}
	if containerd != "" {
		v.Server.Components = append(v.Server.Components, struct{ Name, Version string }{"containerd", containerd})
	}
	return v
}

// W1-1: the floors REFUSE below the CVE-fixed releases (they only warned
// before this ticket closed); an unparseable version warns, never refuses.
func TestCheckFloors(t *testing.T) {
	cases := []struct {
		name       string
		info       dockerVersionInfo
		wantRefuse []string // substrings, one per refusal
		wantWarn   []string
	}{
		{"current stack", versionInfo("29.8.0", "1.5.1", "2.2.0"), nil, nil},
		{"the probe box", versionInfo("29.8.0", "1.5.1", "2.1.5"), nil, nil},
		{"engine below floor", versionInfo("28.0.0", "1.5.1", "2.1.0"), []string{"engine 28.0.0"}, nil},
		{"runc below floor", versionInfo("29.8.0", "1.3.2", "2.1.0"), []string{"runc 1.3.2"}, nil},
		{"containerd below floor", versionInfo("29.8.0", "1.5.1", "1.7.26"), []string{"containerd 1.7.26"}, nil},
		{"all three", versionInfo("1.13.1", "1.0.0", "1.4.0"), []string{"engine", "runc", "containerd"}, nil},
		{"engine behind latest warns", versionInfo("29.5.0", "1.5.1", "2.1.0"), nil, []string{"latest known"}},
		{"unparseable engine warns", versionInfo("dev", "1.5.1", "2.1.0"), nil, []string{"could not be parsed"}},
		{"no runc component warns", versionInfo("29.8.0", "", "2.1.0"), nil, []string{"runc component"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := checkFloors(c.info)
			if len(g.refuse) != len(c.wantRefuse) {
				t.Fatalf("refusals %v, want %d (%v)", g.refuse, len(c.wantRefuse), c.wantRefuse)
			}
			for i, w := range c.wantRefuse {
				if !strings.Contains(g.refuse[i], w) {
					t.Errorf("refusal %d = %q, want it to mention %q", i, g.refuse[i], w)
				}
			}
			if len(g.warn) != len(c.wantWarn) {
				t.Fatalf("warnings %v, want %d (%v)", g.warn, len(c.wantWarn), c.wantWarn)
			}
			for i, w := range c.wantWarn {
				if !strings.Contains(g.warn[i], w) {
					t.Errorf("warning %d = %q, want it to mention %q", i, g.warn[i], w)
				}
			}
		})
	}
}

func TestCheckDesktopFloor(t *testing.T) {
	if g := checkDesktopFloor(""); len(g.refuse)+len(g.warn) != 0 {
		t.Errorf("no Desktop: expected nothing, got %+v", g)
	}
	if g := checkDesktopFloor("4.44.2"); len(g.refuse) != 1 || !strings.Contains(g.refuse[0], "CVE-2025-9074") {
		t.Errorf("4.44.2: expected the CVE refusal, got %+v", g)
	}
	if g := checkDesktopFloor("4.50.0"); len(g.refuse) != 0 || len(g.warn) != 1 || !strings.Contains(g.warn[0], warnDesktopVersion) {
		t.Errorf("4.50.0: expected a warning below %s, got %+v", warnDesktopVersion, g)
	}
	if g := checkDesktopFloor("4.92.0"); len(g.refuse) != 0 || len(g.warn) != 1 || !strings.Contains(g.warn[0], "latest") {
		t.Errorf("4.92.0 (the probe box): expected only a latest-release warning, got %+v", g)
	}
	if g := checkDesktopFloor("4.93.0"); len(g.refuse)+len(g.warn) != 0 {
		t.Errorf("4.93.0: expected nothing, got %+v", g)
	}
	if g := checkDesktopFloor("beta"); len(g.refuse) != 0 || len(g.warn) != 1 {
		t.Errorf("unparseable: expected one warning, got %+v", g)
	}
}

// A `docker info` document from the probe box (trimmed): seccomp with the
// builtin profile, cgroupns, the containerd image store, runc and nvidia
// registered.
const probeBoxInfo = `{
 "SecurityOptions": ["name=seccomp,profile=builtin", "name=cgroupns"],
 "Runtimes": {"io.containerd.runc.v2": {"path": "runc"}, "nvidia": {"path": "nvidia-container-runtime"}, "runc": {"path": "runc"}},
 "DefaultRuntime": "runc",
 "Driver": "overlayfs",
 "DriverStatus": [["driver-type", "io.containerd.snapshotter.v1"]],
 "KernelVersion": "6.18.33.2-microsoft-standard-WSL2",
 "OperatingSystem": "Docker Desktop",
 "OSType": "linux",
 "Architecture": "x86_64"
}`

func TestEngineFacts(t *testing.T) {
	var f engineFacts
	if err := json.Unmarshal([]byte(probeBoxInfo), &f); err != nil {
		t.Fatal(err)
	}
	if !f.hasSecurityOption("seccomp") || f.seccompProfile() != "builtin" {
		t.Errorf("seccomp: has=%v profile=%q", f.hasSecurityOption("seccomp"), f.seccompProfile())
	}
	if f.hasSecurityOption("userns") {
		t.Error("the probe box has no ECI; userns must read false")
	}
	if f.lsm() != "none" {
		t.Errorf("lsm = %q, want none (Desktop's VM has no AppArmor or SELinux)", f.lsm())
	}
	if got := f.imageStore(); !strings.HasPrefix(got, "containerd") {
		t.Errorf("imageStore = %q", got)
	}
	if got := strings.Join(f.runtimeNames(), ","); got != "io.containerd.runc.v2,nvidia,runc" {
		t.Errorf("runtimeNames = %q", got)
	}
	g := checkFacts(f)
	if len(g.refuse) != 0 {
		t.Errorf("the probe box must pass the feature gates, got refusals %v", g.refuse)
	}
	if len(g.warn) != 1 || !strings.Contains(g.warn[0], "nvidia") {
		t.Errorf("expected the nvidia note, got %v", g.warn)
	}

	noSeccomp := engineFacts{SecurityOptions: []string{"name=cgroupns"}, OSType: "linux"}
	if g := checkFacts(noSeccomp); len(g.refuse) != 1 || !strings.Contains(g.refuse[0], "seccomp") {
		t.Errorf("no seccomp must refuse, got %+v", g)
	}
	windowsContainers := engineFacts{SecurityOptions: []string{"name=seccomp"}, OSType: "windows"}
	if g := checkFacts(windowsContainers); len(g.refuse) != 1 || !strings.Contains(g.refuse[0], "Linux engine") {
		t.Errorf("Windows containers must refuse, got %+v", g)
	}
}

// W1-1 / TEN-439: only success is cached; a failure is retried with back-off,
// never held for the life of the process.
func TestProbeCache(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	c := &probeCache{now: func() time.Time { return now }}

	if _, _, ok := c.cached(); ok {
		t.Fatal("an empty cache must not answer")
	}
	c.recordErr(errors.New("down"))
	if _, err, ok := c.cached(); !ok || err == nil {
		t.Fatal("a fresh failure is served from the cache during its back-off")
	}
	now = now.Add(probeBackoffBase + time.Millisecond)
	if _, _, ok := c.cached(); ok {
		t.Fatal("after the back-off the probe must run again")
	}
	c.recordErr(errors.New("still down"))
	if got := c.backoff(); got != 2*probeBackoffBase {
		t.Fatalf("second failure back-off = %v, want %v", got, 2*probeBackoffBase)
	}
	for i := 0; i < 10; i++ {
		c.recordErr(errors.New("down"))
	}
	if got := c.backoff(); got != probeBackoffMax {
		t.Fatalf("back-off must cap at %v, got %v", probeBackoffMax, got)
	}

	f := &probeFacts{endpoint: "x"}
	c.recordOK(f)
	if got, err, ok := c.cached(); !ok || err != nil || got != f {
		t.Fatal("a success is served from the cache")
	}
	if c.failures != 0 {
		t.Error("a success resets the failure count")
	}
	now = now.Add(probeSuccessTTL)
	if _, _, ok := c.cached(); ok {
		t.Fatal("a success expires after its TTL")
	}
}

func TestProbeFactsSummary(t *testing.T) {
	f := probeFacts{version: versionInfo("29.8.0", "1.5.1", "2.1.5"), desktop: "4.92.0", wsl: "2.5.10.0", wslKernel: "6.18.33.2",
		lsm: "none", seccomp: "builtin", imageStore: "containerd (overlayfs)", endpoint: windowsDesktopPipe}
	s := f.summary()
	for _, want := range []string{"engine 29.8.0", "runc 1.5.1", "containerd 2.1.5", "Docker Desktop 4.92.0", "WSL 2.5.10.0 kernel 6.18.33.2",
		"userns off (no ECI)", "LSM none", "seccomp builtin", "image store containerd", "endpoint npipe"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q: %s", want, s)
		}
	}
}
