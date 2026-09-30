package sandbox

import (
	"strings"
	"testing"
)

func TestDockerNetwork_FailsClosed(t *testing.T) {
	cases := map[NetworkMode]string{
		NetInherit:   "none", // an entry naming a backend resolves to inherit → none
		NetAllowlist: "none", // no proxy yet → none
		NetNone:      "none",
		NetFull:      "bridge",
	}
	for mode, want := range cases {
		if got := dockerNetwork(mode); got != want {
			t.Errorf("dockerNetwork(%v) = %q, want %q", mode, got, want)
		}
	}
}

func TestEnvNamesFor_DropsHostAndDangerousNames(t *testing.T) {
	e := EnvPolicy{
		Pass: []string{
			"TENANT_API_KEY",                                   // kept
			"PATH", "HOME", "TMP", "SystemRoot", "USERPROFILE", // host-only, dropped
			"DOCKER_HOST",            // dropped: the schema forbids DOCKER_*
			"NVIDIA_VISIBLE_DEVICES", // dropped: would request a GPU
			"BAD NAME",               // dropped: not a valid name
			"TENANT_API_KEY",         // duplicate, dropped
		},
		Add: []string{"TENANT_ACTIVITY_ID=abc", "TENANT_TRACE=xyz"},
	}
	got := envNamesFor(e)
	want := map[string]bool{"TENANT_API_KEY": true, "TENANT_ACTIVITY_ID": true, "TENANT_TRACE": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected env name %q crossed into the container", n)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	type tc struct {
		a, b string
		want int
		ok   bool
	}
	for _, c := range []tc{
		{"29.8.0", "29.4.3", 1, true},
		{"29.4.3", "29.4.3", 0, true},
		{"29.4.2", "29.4.3", -1, true},
		{"1.4.0-rc.3", "1.2.8", 1, true},
		{"v1.2.8", "1.2.8", 0, true},
		{"4.44.3", "4.76.0", -1, true},
		{"", "1.0.0", 0, false},
		{"unknown", "1.0.0", 0, false},
	} {
		got, ok := compareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("compareVersions(%q,%q) = (%d,%v), want (%d,%v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestCheckFloors(t *testing.T) {
	below := dockerVersionInfo{}
	below.Server.Version = "28.0.0"
	below.Server.Components = []struct {
		Name    string
		Version string
	}{{Name: "runc", Version: "1.1.0"}}
	warn := checkFloors(below)
	if !strings.Contains(warn, "engine") || !strings.Contains(warn, "runc") {
		t.Errorf("expected engine and runc warnings, got %q", warn)
	}

	ok := dockerVersionInfo{}
	ok.Server.Version = "29.8.0"
	ok.Server.Components = []struct {
		Name    string
		Version string
	}{{Name: "runc", Version: "1.5.1"}}
	if warn := checkFloors(ok); warn != "" {
		t.Errorf("expected no warning for a current stack, got %q", warn)
	}
}

func TestWindowsProgram(t *testing.T) {
	for _, cmd := range []string{"cmd.exe /c ver", "powershell -Command x", "run foo.exe", "x.bat"} {
		if _, ok := windowsProgram(Command{Shell: cmd}); !ok {
			t.Errorf("expected %q to be flagged as a Windows program", cmd)
		}
	}
	for _, cmd := range []string{"ls -la", "echo hi", "python3 script.py"} {
		if line, ok := windowsProgram(Command{Shell: cmd}); ok {
			t.Errorf("did not expect %q to be flagged (got %q)", cmd, line)
		}
	}
}

func TestContainerDest(t *testing.T) {
	cases := []struct {
		host, goos, want string
	}{
		{`C:\Users\x\work`, "windows", "/mnt/c/Users/x/work"},
		{`D:\data`, "windows", "/mnt/d/data"},
		{"/home/u/work", "linux", "/home/u/work"},
		{"/Users/u/work", "darwin", "/Users/u/work"},
	}
	for _, c := range cases {
		if got := containerDest(c.host, c.goos); got != c.want {
			t.Errorf("containerDest(%q,%q) = %q, want %q", c.host, c.goos, got, c.want)
		}
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 10}
	b.Write([]byte("hello"))
	b.Write([]byte("world!!!")) // total 13 > 10
	if !b.Truncated() {
		t.Error("expected truncated")
	}
	s := b.String()
	if !strings.HasPrefix(s, "helloworld") || !strings.Contains(s, "truncated") {
		t.Errorf("unexpected capped output %q", s)
	}
	if b.total != 13 {
		t.Errorf("total = %d, want 13", b.total)
	}

	// No cap: everything is kept, nothing marked truncated.
	u := &cappedBuffer{max: 0}
	u.Write([]byte(strings.Repeat("x", 100)))
	if u.Truncated() || len(u.String()) != 100 {
		t.Errorf("uncapped buffer altered output: truncated=%v len=%d", u.Truncated(), len(u.String()))
	}
}

func TestAttributeContainer(t *testing.T) {
	rofs := Result{ExitCode: 1, Output: "sh: cannot create /x: Read-only file system"}
	if v, ok := attributeContainer(rofs, containerSpec{network: "none"}); !ok || v.Kind != "filesystem" {
		t.Errorf("expected filesystem violation, got %+v ok=%v", v, ok)
	}
	oom := Result{ExitCode: 137, Output: "Killed"}
	if v, ok := attributeContainer(oom, containerSpec{network: "none"}); !ok || v.Kind != "limit" {
		t.Errorf("expected limit violation, got %+v ok=%v", v, ok)
	}
	netFail := Result{ExitCode: 6, Output: "curl: (6) Could not resolve host: example.com"}
	if v, ok := attributeContainer(netFail, containerSpec{network: "none"}); !ok || v.Kind != "network" {
		t.Errorf("expected network violation under none, got %+v ok=%v", v, ok)
	}
	// The same network failure with a bridge is NOT attributed to the sandbox.
	if _, ok := attributeContainer(netFail, containerSpec{network: "bridge"}); ok {
		t.Error("did not expect a network violation when the bridge is up")
	}
	// An ordinary non-zero exit is not attributed (fails closed, not escalated).
	if _, ok := attributeContainer(Result{ExitCode: 2, Output: "grep: no match"}, containerSpec{network: "none"}); ok {
		t.Error("did not expect a violation for an ordinary non-zero exit")
	}
}

func TestValidImageRef(t *testing.T) {
	for _, ok := range []string{"tenant-sandbox:latest", "tenant-sandbox@sha256:" + strings.Repeat("a", 64), "debian:bookworm-slim"} {
		if err := validImageRef(ok); err != nil {
			t.Errorf("validImageRef(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"--privileged", "has space", "with\"quote", "-flag"} {
		if err := validImageRef(bad); err == nil {
			t.Errorf("validImageRef(%q) = nil, want error", bad)
		}
	}
}
